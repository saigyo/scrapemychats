package browser

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Channel names accepted by --browser-channel, matching the choices in
// export_chats.py:700-702.
const (
	ChannelChrome = "chrome"
	ChannelEdge   = "msedge"
)

// homeBase is the pseudo environment variable used in the path tables below
// to mean "the user's home directory". It cannot collide with a real
// variable name because a NUL byte is not legal in one.
const homeBase = "\x00home"

// pathEntry is one candidate location for a browser executable.
//
// Base is the name of an environment variable (e.g. "ProgramFiles"), or
// homeBase for the user's home directory, or "" when Rel is already an
// absolute path. When Base is set but unset in the environment, the entry is
// skipped.
//
// Rel is joined to Base with the separator of the *target* OS, not the OS the
// code happens to run on, so candidatePaths is deterministic in tests.
type pathEntry struct {
	Base string
	Rel  string
}

// The path tables are package variables so tests can assert on them without
// re-stating the literals.
var (
	// macChromePaths mirrors where Google Chrome installs on macOS: the
	// system-wide /Applications, and the per-user ~/Applications that
	// Chrome uses when installed without admin rights.
	macChromePaths = []pathEntry{
		{Rel: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"},
		{Base: homeBase, Rel: "Applications/Google Chrome.app/Contents/MacOS/Google Chrome"},
	}
	macEdgePaths = []pathEntry{
		{Rel: "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"},
		{Base: homeBase, Rel: "Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"},
	}

	winChromePaths = []pathEntry{
		{Base: "ProgramFiles", Rel: `Google\Chrome\Application\chrome.exe`},
		{Base: "ProgramFiles(x86)", Rel: `Google\Chrome\Application\chrome.exe`},
		{Base: "LocalAppData", Rel: `Google\Chrome\Application\chrome.exe`},
	}
	// Edge is 32-bit-installed by default even on 64-bit Windows, so
	// ProgramFiles(x86) comes first here.
	winEdgePaths = []pathEntry{
		{Base: "ProgramFiles(x86)", Rel: `Microsoft\Edge\Application\msedge.exe`},
		{Base: "ProgramFiles", Rel: `Microsoft\Edge\Application\msedge.exe`},
	}

	// PATH lookups, tried after the well-known absolute locations.
	winChromeNames = []string{"chrome.exe", "chrome"}
	winEdgeNames   = []string{"msedge.exe", "msedge"}

	// Linux is a developer convenience only: the tool targets the desktop
	// Chrome/Edge that ChatGPT users actually log in with.
	linuxChromeNames = []string{"google-chrome", "google-chrome-stable", "chromium"}
	linuxEdgeNames   = []string{"microsoft-edge"}
)

// candidatePaths returns the ordered list of executables to try for goos.
//
// Entries are either absolute paths or bare program names to resolve via
// $PATH; FindBrowser treats both uniformly. prefer ("chrome", "msedge" or
// "") decides which browser's group comes first; anything else is treated as
// "chrome", matching the default in export_chats.py:700.
//
// env and home are injected so the function is pure and testable for every
// OS from any OS.
func candidatePaths(goos string, env func(string) string, home, prefer string) []string {
	var chrome, edge []pathEntry
	var chromeNames, edgeNames []string

	switch goos {
	case "darwin":
		chrome, edge = macChromePaths, macEdgePaths
	case "windows":
		chrome, edge = winChromePaths, winEdgePaths
		chromeNames, edgeNames = winChromeNames, winEdgeNames
	default:
		chromeNames, edgeNames = linuxChromeNames, linuxEdgeNames
	}

	chromeGroup := append(expand(goos, env, home, chrome), chromeNames...)
	edgeGroup := append(expand(goos, env, home, edge), edgeNames...)

	if prefer == ChannelEdge {
		return append(edgeGroup, chromeGroup...)
	}
	return append(chromeGroup, edgeGroup...)
}

// expand resolves a path table into concrete paths, dropping entries whose
// base directory is not set in the environment.
func expand(goos string, env func(string) string, home string, entries []pathEntry) []string {
	sep := "/"
	if goos == "windows" {
		sep = `\`
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		switch {
		case e.Base == "":
			out = append(out, e.Rel)
		case e.Base == homeBase:
			if home == "" {
				continue
			}
			out = append(out, strings.TrimRight(home, `/\`)+sep+e.Rel)
		default:
			base := lookupEnv(env, e.Base)
			if base == "" {
				continue
			}
			out = append(out, strings.TrimRight(base, `/\`)+sep+e.Rel)
		}
	}
	return out
}

// lookupEnv reads name, falling back to its all-uppercase spelling. Windows
// environment variables are case-insensitive but Go maps are not, and the
// canonical spellings differ between variables ("ProgramFiles" vs
// "LOCALAPPDATA"), so both are tried.
func lookupEnv(env func(string) string, name string) string {
	if v := env(name); v != "" {
		return v
	}
	if up := strings.ToUpper(name); up != name {
		return env(up)
	}
	return ""
}

// ErrBrowserNotFound is returned by FindBrowser when neither Chrome nor Edge
// could be located.
var ErrBrowserNotFound = errors.New("no supported browser found")

// FindBrowser returns the path to the browser executable to drive.
//
// prefer is the --browser-channel value: "chrome" (the default), "msedge",
// or "" for the default. The preferred browser's locations are tried first,
// the other one second, so a user with only Edge installed still gets a
// working run.
func FindBrowser(prefer string) (string, error) {
	home, _ := os.UserHomeDir()
	for _, c := range candidatePaths(runtime.GOOS, os.Getenv, home, prefer) {
		if p, ok := resolveExec(c); ok {
			return p, nil
		}
	}
	return "", fmt.Errorf(
		"%w: looked for Google Chrome and Microsoft Edge in the usual places "+
			"and on $PATH. Install Chrome (https://www.google.com/chrome/) or "+
			"Edge (https://www.microsoft.com/edge), or pass --browser-channel "+
			"to pick the one you have",
		ErrBrowserNotFound)
}

// resolveExec accepts either an absolute path to an existing regular file or
// a program name resolvable on $PATH.
func resolveExec(candidate string) (string, bool) {
	if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
		return candidate, true
	}
	if p, err := exec.LookPath(candidate); err == nil && p != "" {
		return p, true
	}
	return "", false
}
