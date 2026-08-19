// Command scrapemychats exports every conversation from a ChatGPT account to a
// local, searchable archive, then builds a self-contained HTML viewer over it.
//
// It is the Go port of export_chats.py's command line, with extra non-developer
// affordances so the binary works when simply double-clicked: it puts its files
// next to the executable (falling back to ~/Documents when that folder is
// read-only), takes a lock so a second run can't clobber the first, wires
// Ctrl+C to a clean, resume-safe shutdown, builds the viewer automatically, and
// waits for Enter before closing so a double-clicked window doesn't vanish.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/gofrs/flock"
	"golang.org/x/term"

	"github.com/saigyo/scrapemychats/internal/browser"
	"github.com/saigyo/scrapemychats/internal/export"
	"github.com/saigyo/scrapemychats/internal/viewer"
)

// version is the build version, stamped at release time via
// -ldflags "-X main.version=..." (see .goreleaser.yaml). It stays "dev" for
// plain `go build` and local runs.
var version = "dev"

// options holds the parsed command line. Path flags default to "" and are
// resolved against the base directory after it is known (see resolvePath), so
// the base-relative defaults can be computed from os.Executable() at runtime.
type options struct {
	csv         string
	out         string
	profile     string
	categories  string
	limit       int
	rediscover  bool
	fixFiles    bool
	channel     string
	viewerOnly  bool
	viewerTitle string
	noPause     bool
	showVersion bool
}

// registerFlags declares every flag on fs and returns the options they write
// into. It is separated from main so the defaults can be unit-tested without a
// live run.
func registerFlags(fs *flag.FlagSet) *options {
	o := &options{}
	fs.StringVar(&o.csv, "csv", "", "chat list CSV (default: chats.csv next to the app; auto-created by discovery if missing)")
	fs.StringVar(&o.out, "out", "", "output directory (default: export next to the app)")
	fs.StringVar(&o.profile, "profile", "", "Chrome profile directory for the saved login (default: browser_profile next to the app)")
	fs.StringVar(&o.categories, "categories", "", "categories.json for the viewer (default: categories.json next to the app)")
	fs.IntVar(&o.limit, "limit", 0, "export at most N chats")
	fs.BoolVar(&o.rediscover, "rediscover", false, "refresh the chat list from the account before exporting")
	fs.BoolVar(&o.fixFiles, "fix-files", false, "don't re-export; revisit exported chats and fetch any missing files")
	fs.StringVar(&o.channel, "browser-channel", browser.ChannelChrome, "installed browser to drive: chrome or msedge")
	fs.BoolVar(&o.viewerOnly, "viewer-only", false, "only (re)build the HTML viewer from an existing export, then exit")
	fs.StringVar(&o.viewerTitle, "viewer-title", "The Chat Archive", "title shown in the viewer")
	fs.BoolVar(&o.noPause, "no-pause", false, "don't wait for Enter before closing (also auto-skipped when not a terminal)")
	fs.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	return o
}

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is main's body, returning a process exit code. It is a function (rather
// than inline in main) so its deferred pause runs before the process exits:
// os.Exit would skip defers, so main only calls os.Exit on run's return value.
func run(args []string) int {
	fs := flag.NewFlagSet("scrapemychats", flag.ContinueOnError)
	o := registerFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// --version: print and exit before anything else (no pause, no browser).
	if o.showVersion {
		fmt.Println("scrapemychats " + version)
		return 0
	}

	// pause is deferred first so it runs LAST (after the browser is closed and
	// the lock released), keeping a double-clicked window open on any exit path.
	defer maybePause(o.noPause)

	// Validate --browser-channel up front, mirroring Python's
	// argparse choices=["chrome","msedge"] (reject anything else with a clear
	// message instead of silently treating it as chrome).
	if o.channel != browser.ChannelChrome && o.channel != browser.ChannelEdge {
		fmt.Fprintf(os.Stderr, "invalid --browser-channel %q: must be %q or %q\n",
			o.channel, browser.ChannelChrome, browser.ChannelEdge)
		return 2
	}

	base, note := resolveBaseDir(mustExecutable(), dirWritable, homeDir())
	if err := os.MkdirAll(base, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot create %s: %v\n", base, err)
		return 1
	}
	if note != "" {
		fmt.Println(note)
	}

	csvPath := resolvePath(o.csv, base, "chats.csv")
	outDir := resolvePath(o.out, base, "export")
	profileDir := resolvePath(o.profile, base, "browser_profile")
	categoriesPath := resolvePath(o.categories, base, "categories.json")

	// --viewer-only: just (re)build the viewer over an existing export.
	if o.viewerOnly {
		if err := viewer.Build(outDir, categoriesPath, o.viewerTitle); err != nil {
			fmt.Fprintf(os.Stderr, "building viewer: %v\n", err)
			return 1
		}
		fmt.Println("Done — open " + filepath.Join(outDir, "viewer.html"))
		return 0
	}

	// One run at a time: a second concurrent run would fight over the browser
	// profile and the output folder.
	release, ok := acquireLock(base)
	if !ok {
		fmt.Println("Another scrapemychats run looks active (lock file present).")
		fmt.Println("If you're sure nothing else is running, delete " + lockPath(base) + " and try again.")
		return 1
	}
	defer release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// After the first Ctrl+C cancels ctx (graceful, resume-safe shutdown),
	// restore the default SIGINT handler so a SECOND Ctrl+C force-kills a stuck
	// process — otherwise NotifyContext would keep swallowing it until run
	// returns, which is worse than the Python.
	go func() {
		<-ctx.Done()
		stop()
	}()
	fmt.Println("(Closing this window or pressing Ctrl+C is safe — re-running resumes where it left off.)")

	execPath, err := browser.FindBrowser(o.channel)
	if err != nil {
		// The error already carries install hints for Chrome and Edge.
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sess, err := browser.Launch(ctx, browser.Options{ProfileDir: profileDir, ExecPath: execPath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "launching browser: %v\n", err)
		return 1
	}
	defer sess.Close()

	b := export.NewSessionBrowser(sess)
	cfg := export.Config{
		CSVPath:    csvPath,
		OutDir:     outDir,
		ProfileDir: profileDir,
		Limit:      o.limit,
		Rediscover: o.rediscover,
	}

	// --fix-files: recover missing files only; no re-export, no viewer rebuild
	// (parity with export_chats.py).
	if o.fixFiles {
		if err := export.FixFilesMode(ctx, b, cfg); err != nil {
			if wasCancelled(ctx, err) {
				fmt.Println("Interrupted — progress is saved, re-run to resume.")
				return 0
			}
			fmt.Fprintf(os.Stderr, "fix-files: %v\n", err)
			return 1
		}
		return 0
	}

	// Default flow: export everything, then build the viewer. On interruption
	// we print the resume-safe message and do NOT print "Done" or treat the
	// half-done export as complete.
	if _, err := export.Export(ctx, b, cfg); err != nil {
		if wasCancelled(ctx, err) {
			fmt.Println("Interrupted — progress is saved, re-run to resume.")
			// Best-effort viewer over whatever was exported so far.
			_ = viewer.Build(outDir, categoriesPath, o.viewerTitle)
			return 0
		}
		fmt.Fprintf(os.Stderr, "export: %v\n", err)
		return 1
	}

	if err := viewer.Build(outDir, categoriesPath, o.viewerTitle); err != nil {
		fmt.Fprintf(os.Stderr, "building viewer: %v\n", err)
		return 1
	}
	fmt.Println("Done — open " + filepath.Join(outDir, "viewer.html"))
	return 0
}

// resolveBaseDir decides where the app keeps its files. It prefers the folder
// the executable sits in (so a double-clicked binary drops everything next to
// itself), and falls back to ~/Documents/scrapemychats when that folder is not
// writable — the common case for a binary in /Applications or Program Files.
//
// writable and home are injected so the decision is a pure, testable function.
// The returned note is non-empty only on the fallback path, explaining the
// redirect to the user.
func resolveBaseDir(exePath string, writable func(string) bool, home string) (string, string) {
	dir := filepath.Dir(exePath)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if writable(dir) {
		return dir, ""
	}
	fallback := filepath.Join(home, "Documents", "scrapemychats")
	note := fmt.Sprintf("The app's folder (%s) isn't writable; keeping your export in %s instead.", dir, fallback)
	return fallback, note
}

// resolvePath returns the base-relative default when the flag was left empty,
// and otherwise honors the value the user gave (an absolute path as-is, a
// relative path against the current directory, standard CLI behavior).
func resolvePath(flagVal, base, name string) string {
	if flagVal == "" {
		return filepath.Join(base, name)
	}
	return flagVal
}

// dirWritable reports whether dir accepts new files, by creating and removing a
// temporary probe file. A directory that doesn't exist or rejects the write is
// treated as not writable.
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".scrapemychats-wtest-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// mustExecutable returns the running executable's path, falling back to
// os.Args[0] if the OS can't report it (rare; keeps resolveBaseDir working).
func mustExecutable() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return os.Args[0]
}

// homeDir returns the user's home directory, or "." if it can't be determined,
// so the ~/Documents fallback still resolves to something usable.
func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "."
}

func lockPath(base string) string { return filepath.Join(base, ".scrapemychats.lock") }

// acquireLock takes an OS advisory lock on base/.scrapemychats.lock so a
// second concurrent run can't fight over the browser profile and output
// folder. It returns a release func and whether the lock was acquired.
//
// The lock is held by the OS for the life of the process and released
// automatically when the process exits — including on a crash — so there is no
// stale lock file to reason about and no age heuristic that could either wrongly
// hijack a run legitimately lasting many hours or let two processes both take
// over the same file. TryLock is atomic, so exactly one of two racing runs
// wins.
func acquireLock(base string) (release func(), ok bool) {
	fl := flock.New(lockPath(base))
	locked, err := fl.TryLock()
	if err != nil || !locked {
		return func() {}, false
	}
	return func() { _ = fl.Unlock() }, true
}

// shouldPause decides whether to wait for Enter before exiting: only when the
// user didn't pass --no-pause AND stdin is an interactive terminal (so piped or
// service invocations never block).
func shouldPause(noPause, stdinIsTTY bool) bool {
	return !noPause && stdinIsTTY
}

// maybePause waits for the user to press Enter when appropriate, so a
// double-clicked window stays open long enough to read the final message.
func maybePause(noPause bool) {
	if !shouldPause(noPause, isTTY(os.Stdin)) {
		return
	}
	fmt.Print("Press Enter to close...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// wasCancelled reports whether err represents an interrupted run: either the
// signal context was cancelled, or the error itself wraps context.Canceled
// (which is what the export loop propagates on SIGINT). Both are checked so a
// cancellation is recognized even if ctx.Err() has been cleared by the time we
// look.
func wasCancelled(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled)
}

// isTTY reports whether f is an interactive terminal, used to decide whether
// the end-of-run pause makes sense. term.IsTerminal is used rather than a raw
// ModeCharDevice check so that character devices which are not terminals
// (notably /dev/null) don't trigger the prompt, and so Windows consoles are
// detected correctly for the double-click flow.
func isTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}
