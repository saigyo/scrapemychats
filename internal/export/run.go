// Package export is the capstone orchestration layer: it drives the browser,
// discovers or reads the chat list, captures every conversation, renders it to
// Markdown, downloads referenced files, and sweeps the account file Library —
// the export loop and fix-files pass of export_chats.py's main()
// (export_chats.py:682-851 and 741-747).
//
// It wires the already-committed internal packages together and adds nothing
// of their logic. The one seam it introduces is the Browser interface: the
// loop talks to the browser only through this small surface, so it can be
// exercised end to end against a scripted fake (no Chrome, no network) — see
// run_test.go. The real implementation, sessionBrowser, wraps a live
// *browser.Session.
package export

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/saigyo/scrapemychats/internal/browser"
	"github.com/saigyo/scrapemychats/internal/convmd"
	"github.com/saigyo/scrapemychats/internal/discover"
	"github.com/saigyo/scrapemychats/internal/files"
	"github.com/saigyo/scrapemychats/internal/fsutil"
)

// Pacing constants, ported exactly from export_chats.py:47-55.
const (
	// DelayMin/DelayMax bound the polite random delay between chats, in
	// seconds (DELAY_RANGE).
	DelayMin = 10.0
	DelayMax = 16.0
	// ExtraDelayPer429 is the permanent slowdown added each time the server
	// throttles us; ExtraDelayCap bounds the accumulated slowdown.
	ExtraDelayPer429 = 5.0
	ExtraDelayCap    = 40.0
	// MaxAttempts is how many non-throttle failures a single chat is retried
	// before it is recorded as failed.
	MaxAttempts = 3
	// LongBreakEvery/LongBreakMin/LongBreakMax define the longer breather
	// taken every N exported chats (LONG_BREAK_EVERY / LONG_BREAK_S).
	LongBreakEvery = 20
	LongBreakMin   = 150.0
	LongBreakMax   = 240.0
)

// RateLimitBackoffs are the escalating cool-down waits (seconds) after a
// "too many requests" response; a throttle is retried without counting as a
// failure. Port of RATE_LIMIT_BACKOFFS_S (export_chats.py:53).
var RateLimitBackoffs = []int{300, 600, 900, 1800, 1800}

// sleep pauses for d, or returns early with ctx.Err() the moment ctx is
// cancelled (nil if the full duration elapsed). It is a package variable so
// tests run the pacing and retry loops without waiting — the fake records the
// durations that would elapse and honors cancellation via ctx.Err(). Making
// the sleep cancellable is what lets a SIGINT during a rate-limit backoff (up
// to 30 minutes) abort promptly instead of stranding the user.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// uniform returns a uniformly random value in [min, max), matching Python's
// random.uniform(min, max). It is a package variable so tests can make the
// random component deterministic (returning min) and assert the pacing math.
var uniform = func(min, max float64) float64 {
	return min + rand.Float64()*(max-min)
}

// seconds converts a floating-point number of seconds into a time.Duration.
func seconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

// Browser is the exact set of browser operations the export loop uses. Keeping
// it this small is what makes the loop testable without a real Chrome: a fake
// implementation scripts conversation JSON/status/errors and hands back a fake
// files.Client. sessionBrowser is the production adapter over *browser.Session.
type Browser interface {
	// EnsureLoggedIn blocks until a signed-in ChatGPT session exists.
	EnsureLoggedIn() error
	// CaptureAuth copies the auth headers the frontend itself uses.
	CaptureAuth() (map[string]string, error)
	// CaptureConversation navigates to a chat and returns its JSON, the auth
	// headers used, and the HTTP status. A non-200 status yields nil data;
	// browser.ErrCaptureTimeout is returned when the response never arrived.
	CaptureConversation(url, cid string) (data map[string]any, auth map[string]string, status int, err error)
	// Client returns the network client the file downloaders run against.
	Client() files.Client
	// Fetcher returns the in-page GET primitive discovery needs.
	Fetcher() browser.Fetcher
}

// Config holds the settings for one export (or fix-files) run.
type Config struct {
	CSVPath    string // chat-list CSV, discovered into if missing
	OutDir     string // output directory (folders, manifest.csv, errors.log)
	ProfileDir string // browser profile dir; unused here, owned by the CLI/launcher
	Limit      int    // export at most N chats (0 = all)
	Rediscover bool   // refresh the CSV from the account before exporting
}

// Summary reports what an Export run did, so the CLI can print the next step.
type Summary struct {
	Exported   int
	Skipped    int
	Failed     int
	ErrorsPath string
}

// Export runs the full export loop: log in, (re)discover, capture every chat,
// render + download, then sweep the Library. Port of main()'s export branch
// (export_chats.py:710-844). The final "Next: build the viewer" line is left
// to the CLI, which owns the viewer step.
//
// ctx carries cancellation (SIGINT): the loop checks it at the top of every
// chat and every sleep returns early when it fires, so an interrupted run stops
// promptly, writes NO manifest row for the chat it was mid-way through, and
// returns ctx.Err() (non-nil) — letting the CLI print its resume-safe
// "Interrupted" message instead of building a viewer over a half-done export.
// Resume markers make re-running pick up exactly where it stopped.
func Export(ctx context.Context, b Browser, cfg Config) (Summary, error) {
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return Summary{}, fmt.Errorf("export: creating %s: %w", cfg.OutDir, err)
	}
	manifestPath := filepath.Join(cfg.OutDir, "manifest.csv")
	errorsPath := filepath.Join(cfg.OutDir, "errors.log")
	newManifest := !fileExists(manifestPath)

	manifestFile, err := os.OpenFile(manifestPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return Summary{}, fmt.Errorf("export: opening %s: %w", manifestPath, err)
	}
	defer manifestFile.Close()
	manifest := csv.NewWriter(manifestFile)

	errFile, err := os.OpenFile(errorsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return Summary{}, fmt.Errorf("export: opening %s: %w", errorsPath, err)
	}
	defer errFile.Close()

	// gerr writes a global (not chat-scoped) error record, exactly as Python's
	// gerr does: "{msg}\n" (export_chats.py:737-739).
	gerr := func(msg string) { fmt.Fprintf(errFile, "%s\n", msg) }

	if newManifest {
		if err := manifest.Write([]string{"url", "title", "folder", "status", "messages", "files_ok", "files_failed"}); err != nil {
			return Summary{}, fmt.Errorf("export: writing manifest header: %w", err)
		}
		manifest.Flush()
	}

	if err := b.EnsureLoggedIn(); err != nil {
		return Summary{}, err
	}

	// (Re)discover the chat list when asked, or when the CSV doesn't exist yet.
	// Python's discover_chats captures auth itself; here CaptureAuth is a
	// browser op and DiscoverChats takes the resulting headers.
	if cfg.Rediscover || !fileExists(cfg.CSVPath) {
		auth, err := b.CaptureAuth()
		if err != nil {
			return Summary{}, err
		}
		if err := discover.DiscoverChats(b.Fetcher(), auth, cfg.CSVPath); err != nil {
			return Summary{}, err
		}
	}

	chats, err := discover.ReadChatList(cfg.CSVPath)
	if err != nil {
		return Summary{}, err
	}
	// Python: chats[:limit] — a limit larger than the list just returns all.
	if cfg.Limit > 0 && cfg.Limit < len(chats) {
		chats = chats[:cfg.Limit]
	}
	fsutil.Log(fmt.Sprintf("%d chats to process", len(chats)))

	client := b.Client()
	exported, skipped, failedN, attempted := 0, 0, 0, 0
	extraDelay := 0.0 // grows every time the server throttles us
	var lastAuth map[string]string

	summaryNow := func() Summary {
		return Summary{Exported: exported, Skipped: skipped, Failed: failedN, ErrorsPath: errorsPath}
	}

	// pace applies the polite delay after a chat has been dealt with, whether
	// it succeeded or failed. A failed chat costs the server exactly the same
	// page navigation(s) as a successful one — more, in fact, once its
	// captureWithRetry retries are counted — so it must be paced the same way.
	// This is a deliberate divergence from export_chats.py:797-801, whose
	// `if not data: ... continue` skips the trailing delay entirely on
	// failure: a run where most chats fail would then hammer the account
	// unthrottled, which is exactly what produced ChatGPT's "slow down" modal
	// in the field when a broken endpoint failed all 42 chats.
	//
	// rlHits is the throttle count for the chat just handled; it grows the
	// permanent slowdown (export_chats.py:834-841). The long breather keys off
	// attempted, not exported: the server sees every attempt, not just the
	// successful ones, so a run of mostly-failing chats must still take its
	// breathers.
	pace := func(rlHits int) error {
		if rlHits > 0 {
			extraDelay = math.Min(extraDelay+ExtraDelayPer429*float64(rlHits), ExtraDelayCap)
			fsutil.Log(fmt.Sprintf("    pace slowed: +%.0fs per chat from now on", extraDelay))
		}
		if attempted%LongBreakEvery == 0 {
			pause := uniform(LongBreakMin, LongBreakMax)
			fsutil.Log(fmt.Sprintf("    taking a %ds breather after %d chats...", int(pause), attempted))
			if err := sleep(ctx, seconds(pause)); err != nil {
				return err
			}
		}
		return sleep(ctx, seconds(uniform(DelayMin, DelayMax)+extraDelay))
	}

	for idx, chat := range chats {
		// Interrupted (SIGINT) before touching this chat: return what's done so
		// far with the cancellation error, writing no spurious "failed" row for
		// the chats we never reached.
		if err := ctx.Err(); err != nil {
			return summaryNow(), err
		}

		i := idx + 1 // Python enumerate(chats, 1)
		folder := filepath.Join(cfg.OutDir, fmt.Sprintf("%03d_%s_%s", i, fsutil.Sanitize(chat.Title), cidHead(chat.ID)))
		if fileExists(filepath.Join(folder, "conversation.json")) {
			skipped++
			continue
		}
		attempted++ // counts chats we actually work on, for the long-breather cadence

		title, url, cid := chat.Title, chat.URL, chat.ID
		// err records a chat-scoped error, exactly as Python's per-chat err
		// closure does: "[{title}] {url}\n    {msg}\n" (export_chats.py:763-765).
		errFn := func(msg string) { fmt.Fprintf(errFile, "[%s] %s\n    %s\n", title, url, msg) }

		fsutil.Log(fmt.Sprintf("[%d/%d] %s", i, len(chats), title))

		data, auth, rlHits, cerr := captureWithRetry(ctx, b, url, cid, errFn)
		// Cancelled mid-capture (or mid-backoff): abort without recording a
		// failed row for this chat — it wasn't a failure, the user interrupted.
		if cerr != nil {
			return summaryNow(), cerr
		}

		// Python's `if not data:` — an empty dict is falsy there too, so a 200
		// that returned {} is recorded as failed just like a missing capture.
		if len(data) == 0 {
			failedN++
			if err := writeManifestRow(manifest, []string{url, title, "", "failed", "0", "0", "0"}); err != nil {
				return Summary{}, fmt.Errorf("export: writing manifest row: %w", err)
			}
			// Pace here too — see the pace closure's doc comment for why a
			// failed chat is not exempt.
			if err := pace(rlHits); err != nil {
				return summaryNow(), err
			}
			continue
		}

		if err := os.MkdirAll(folder, 0o755); err != nil {
			return Summary{}, fmt.Errorf("export: creating %s: %w", folder, err)
		}
		if err := os.WriteFile(filepath.Join(folder, "conversation.md"),
			[]byte(convmd.RenderMarkdown(data, url, title)), 0o644); err != nil {
			return Summary{}, fmt.Errorf("export: writing conversation.md: %w", err)
		}

		// The conversation is already a parsed map here (CaptureConversation
		// unmarshaled it), so the original mapping document order is gone;
		// CollectFileRefs/CollectSandboxRefs fall back to sorted key order,
		// which is exactly what re-marshaling the map would yield since Go's
		// json.Marshal emits map keys sorted. The reference SET is identical to
		// Python's either way — only the download attempt order (and thus any
		// collision-rename suffix) could differ.
		refs := files.CollectFileRefs(data, nil)
		srefs := files.CollectSandboxRefs(data, nil)
		filesDir := filepath.Join(folder, "files")
		fOK, fFail := 0, 0
		if len(refs) > 0 || len(srefs) > 0 {
			fsutil.Log(fmt.Sprintf("    %d file(s) + %d generated file(s), downloading...", len(refs), len(srefs)))
			ok, fail := files.DownloadFiles(client, refs, filesDir, auth, errFn)
			sOK, sFail := files.DownloadSandboxFiles(client, cid, srefs, filesDir, auth, errFn)
			fOK, fFail = ok+sOK, fail+sFail
		}

		// Cancelled (SIGINT) during the downloads? The download helpers turn a
		// cancelled in-page fetch into failed-file counts rather than an error,
		// so bail out here BEFORE writing the conversation.json marker — marking
		// the chat complete now would make a normal resume skip it with files
		// still missing. The next run redoes this chat; files already on disk
		// are skipped via AlreadyHave.
		if err := ctx.Err(); err != nil {
			return summaryNow(), err
		}

		lastAuth = auth
		nMsgs := len(convmd.OrderedMessages(data))

		// Written LAST: conversation.json is the resume/completion marker. The
		// key order and HTML-escaping differ from Python's
		// json.dumps(indent=1) — Go sorts map keys and (via the Encoder with
		// SetEscapeHTML(false)) leaves <,>,& unescaped — but both tools only
		// ever read this file back through a JSON parser, so the decoded
		// document is identical; it is an intermediate/marker file, not a
		// rendered artifact.
		convJSON, err := marshalConversationJSON(data)
		if err != nil {
			return Summary{}, fmt.Errorf("export: encoding conversation.json: %w", err)
		}
		// Write the resume marker atomically: resume only checks that
		// conversation.json exists, so a partial write (disk full, crash, a
		// forced second Ctrl+C) must never leave truncated JSON in place — that
		// would make every later run skip the chat forever. Write to a temp
		// file and rename, which is atomic on the same filesystem.
		if err := fsutil.WriteFileAtomic(filepath.Join(folder, "conversation.json"), convJSON, 0o644); err != nil {
			return Summary{}, fmt.Errorf("export: writing conversation.json: %w", err)
		}

		// conversation.json (the resume marker) is already on disk, so a failure
		// to record the manifest row means the row is missing for a chat that
		// will be skipped on resume. That's an inconsistency we surface rather
		// than swallow: abort with the I/O error (typically disk-full) instead
		// of reporting a success that didn't happen.
		if err := writeManifestRow(manifest, []string{url, title, filepath.Base(folder), "ok",
			strconv.Itoa(nMsgs), strconv.Itoa(fOK), strconv.Itoa(fFail)}); err != nil {
			return Summary{}, fmt.Errorf("export: writing manifest row: %w", err)
		}
		exported++

		if err := pace(rlHits); err != nil {
			return summaryNow(), err
		}
	}

	// Library sweep, using the last successful auth or a fresh capture. Python:
	// library_sweep(..., last_auth or capture_auth(page), gerr).
	sweepAuth := lastAuth
	if len(sweepAuth) == 0 {
		a, err := b.CaptureAuth()
		if err != nil {
			// Python would let capture_auth raise and abort; the export folders
			// are already written (resume-safe), so here the sweep is skipped
			// with a logged note rather than discarding a completed run.
			fsutil.Log(fmt.Sprintf("  ! skipping library sweep: could not capture auth: %v", err))
			return finish(exported, skipped, failedN, errorsPath), nil
		}
		sweepAuth = a
	}
	files.LibrarySweep(client, cfg.OutDir, sweepAuth, gerr)

	return finish(exported, skipped, failedN, errorsPath), nil
}

// captureWithRetry runs the per-chat retry state machine, an exact port of
// export_chats.py:767-796. It returns the captured conversation (nil on
// failure), the auth headers that came with it, the throttle count the caller's
// pacing step uses to grow the permanent slowdown (Python's rl_hits, still in
// scope at that step), and a non-nil error ONLY when ctx was cancelled.
//
// Cancellation is distinguished from a normal transport error: on SIGINT,
// browser.CaptureConversation returns the session's ctx.Err() (context.Canceled
// — NOT ErrCaptureTimeout), which must not be counted as a retry attempt or
// logged as a per-chat failure. It aborts the retry loop and propagates, so the
// caller can stop the whole run. A sleep cut short by cancellation does the same.
func captureWithRetry(ctx context.Context, b Browser, url, cid string, errFn func(string)) (map[string]any, map[string]string, int, error) {
	var data map[string]any
	var auth map[string]string
	attempt, rlHits := 0, 0
	for attempt < MaxAttempts && rlHits < len(RateLimitBackoffs)+1 {
		if err := ctx.Err(); err != nil {
			return data, auth, rlHits, err
		}
		d, a, status, cerr := b.CaptureConversation(url, cid)
		if cerr != nil {
			// Cancellation is not a failure and not a retry attempt.
			if ctx.Err() != nil || errors.Is(cerr, context.Canceled) || errors.Is(cerr, context.DeadlineExceeded) {
				return data, auth, rlHits, cerr
			}
			if errors.Is(cerr, browser.ErrCaptureTimeout) {
				attempt++
				errFn(fmt.Sprintf("timeout waiting for conversation JSON (attempt %d)", attempt))
				fsutil.Log("    timed out (Cloudflare check? solve it in the window if shown)")
				if err := sleep(ctx, 15*time.Second); err != nil {
					return data, auth, rlHits, err
				}
				continue
			}
			attempt++
			errFn(fmt.Sprintf("%v (attempt %d)", cerr, attempt))
			if err := sleep(ctx, 10*time.Second); err != nil {
				return data, auth, rlHits, err
			}
			continue
		}
		if status == 200 {
			data, auth = d, a
			break
		}
		if status == 429 || status == 403 {
			// Throttled: cool down with escalating waits, not counted against
			// the normal retry attempts.
			wait := RateLimitBackoffs[min(rlHits, len(RateLimitBackoffs)-1)]
			rlHits++
			fsutil.Log(fmt.Sprintf("    rate limited (HTTP %d), cooling down %d min...", status, wait/60))
			if err := sleep(ctx, time.Duration(wait)*time.Second); err != nil {
				return data, auth, rlHits, err
			}
			continue
		}
		attempt++
		errFn(fmt.Sprintf("conversation HTTP %d (attempt %d)", status, attempt))
		if err := sleep(ctx, 10*time.Second); err != nil {
			return data, auth, rlHits, err
		}
	}
	return data, auth, rlHits, nil
}

// FixFilesMode revisits every exported chat and downloads whatever files are
// still missing, then sweeps the Library. Port of main()'s fix-files branch
// (export_chats.py:741-747). It does not re-export conversations and does not
// rebuild the viewer, matching the Python.
//
// ctx is honored at the boundaries before each long operation (login, the
// fix-files pass, the sweep). files.FixFiles/LibrarySweep are opaque loops in
// the files package, so cancellation is coarse-grained here rather than
// per-folder; FixFilesMode writes no manifest rows, so there is nothing
// spurious to leave behind on interruption.
func FixFilesMode(ctx context.Context, b Browser, cfg Config) error {
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return fmt.Errorf("export: creating %s: %w", cfg.OutDir, err)
	}
	errorsPath := filepath.Join(cfg.OutDir, "errors.log")
	errFile, err := os.OpenFile(errorsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("export: opening %s: %w", errorsPath, err)
	}
	defer errFile.Close()

	if err := b.EnsureLoggedIn(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	auth, err := b.CaptureAuth()
	if err != nil {
		return err
	}

	client := b.Client()
	// files.FixFiles formats each record itself and hands the finished string
	// to ef; gerr for the sweep writes a bare "{msg}\n".
	ef := func(record string) { fmt.Fprint(errFile, record) }
	gerr := func(msg string) { fmt.Fprintf(errFile, "%s\n", msg) }

	if err := ctx.Err(); err != nil {
		return err
	}
	files.FixFiles(client, cfg.OutDir, auth, ef)
	if err := ctx.Err(); err != nil {
		return err
	}
	files.LibrarySweep(client, cfg.OutDir, auth, gerr)
	fsutil.Log("fix-files pass complete.")
	return nil
}

// finish logs the run summary (export_chats.py:847-850, minus the "Next" line
// the CLI owns) and returns the counts.
func finish(exported, skipped, failedN int, errorsPath string) Summary {
	fsutil.Log("")
	fsutil.Log(fmt.Sprintf("Done. exported=%d skipped(already done)=%d failed=%d", exported, skipped, failedN))
	if failedN > 0 {
		fsutil.Log(fmt.Sprintf("Failures listed in %s — re-run to retry just those chats.", errorsPath))
	}
	return Summary{Exported: exported, Skipped: skipped, Failed: failedN, ErrorsPath: errorsPath}
}

// writeManifestRow writes and immediately flushes one manifest row, matching
// Python's per-row mf.flush() so an interrupted run leaves a consistent
// manifest on disk. It returns any write/flush error (e.g. disk full) so the
// caller can stop rather than report a success that never reached disk.
func writeManifestRow(w *csv.Writer, row []string) error {
	if err := w.Write(row); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}

// marshalConversationJSON encodes the conversation as indented JSON for the
// resume marker. SetEscapeHTML(false) keeps <, > and & literal; the trailing
// newline the Encoder appends is trimmed.
func marshalConversationJSON(data map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(data); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// cidHead returns the first 8 characters of a conversation id, matching
// Python's cid[:8]. Discovery guarantees a full UUID, but a shorter id is
// returned whole rather than panicking (Python's slice would do the same).
func cidHead(cid string) string {
	if len(cid) > 8 {
		return cid[:8]
	}
	return cid
}

// fileExists reports whether a plain filesystem entry exists at path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
