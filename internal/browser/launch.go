package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// Network buffer sizes requested when enabling the Network domain.
//
// Chrome evicts response bodies from its per-target buffer as soon as the
// buffer fills, and Network.getResponseBody then fails with "No data found
// for resource with given identifier". Playwright kept bodies alive for us;
// with raw CDP we have to ask for a buffer big enough to survive a
// conversation page's worth of traffic. capture.go additionally has a
// re-fetch fallback for when eviction happens anyway.
const (
	maxTotalBufferSize    = 256 * 1024 * 1024
	maxResourceBufferSize = 64 * 1024 * 1024
)

// Options configures Launch. The --browser-channel choice has already been
// resolved to an executable path by FindBrowser.
type Options struct {
	// ProfileDir is the persistent Chrome user-data directory that holds
	// the saved ChatGPT login (--profile in export_chats.py:690).
	ProfileDir string
	// ExecPath is the browser executable, as returned by FindBrowser.
	ExecPath string
}

// flagSpec is one Chrome command line flag. A bool value means a valueless
// --flag (when true); a string value means --flag=value. This mirrors how
// chromedp.Flag encodes its argument.
type flagSpec struct {
	Name  string
	Value any
}

// allocatorFlags returns the complete, ordered set of Chrome flags used to
// launch the browser. It is deliberately built from scratch instead of from
// chromedp.DefaultExecAllocatorOptions.
//
// Anti-detection contract (see launch_test.go, which enforces all of it):
//
//   - --disable-blink-features=AutomationControlled: the one flag
//     export_chats.py:732 passes; it stops Blink from advertising itself as
//     automated (navigator.webdriver).
//   - --user-data-dir: the persistent profile, so the login is reused.
//   - --no-first-run / --no-default-browser-check: suppress the first-run
//     and default-browser dialogs, which would otherwise sit on top of the
//     login window. Playwright passes both too.
//   - NO --headless: ChatGPT blocks headless browsers outright.
//   - NO --enable-automation: it sets the "Chrome is being controlled by
//     automated test software" bit that bot detection looks for. chromedp's
//     defaults would add it, which is exactly why they are not used.
//   - no window-size/emulation flags: mirrors no_viewport=True in
//     export_chats.py:731, leaving the window exactly as a human's would be.
//
// (chromedp itself always appends --remote-debugging-port=0 and the initial
// about:blank URL; those are the transport, not behaviour we choose.)
func allocatorFlags(profileDir string) []flagSpec {
	return []flagSpec{
		{Name: "user-data-dir", Value: profileDir},
		{Name: "disable-blink-features", Value: "AutomationControlled"},
		{Name: "no-first-run", Value: true},
		{Name: "no-default-browser-check", Value: true},
	}
}

// allocatorOptions turns Options into the chromedp allocator options,
// deriving every flag from allocatorFlags so the tested list is the list
// that is actually used.
func allocatorOptions(opts Options) []chromedp.ExecAllocatorOption {
	out := []chromedp.ExecAllocatorOption{chromedp.ExecPath(opts.ExecPath)}
	for _, f := range allocatorFlags(opts.ProfileDir) {
		out = append(out, chromedp.Flag(f.Name, f.Value))
	}
	return out
}

// chromedpErrorf routes chromedp's internal error log. chromedp prints an
// "unhandled ... event: ..." notice for every CDP event it does not model —
// e.g. dom.EventTopLayerElementsUpdated, which modern Chrome emits for
// popovers/dialogs/<dialog> top-layer changes on ordinary pages. These are
// harmless and only alarm the user (they read as "ERROR"), so they are
// dropped; anything else is a genuine problem and is forwarded to stderr.
func chromedpErrorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if isBenignChromedpNoise(msg) {
		return
	}
	log.Printf("chromedp: %s", msg)
}

// isBenignChromedpNoise reports whether msg is one of chromedp's informational
// "unhandled ... event" notices, which are safe to discard.
func isBenignChromedpNoise(msg string) bool {
	return strings.Contains(msg, "unhandled") && strings.Contains(msg, "event")
}

// Session is a running, headed browser plus the page (CDP target) the export
// drives. It is not safe for concurrent use by multiple goroutines beyond
// what the individual functions in this package do internally.
type Session struct {
	ctx        context.Context
	cancels    []context.CancelFunc
	ExecPath   string
	ProfileDir string

	// ua caches the browser's navigator.userAgent (see UserAgent).
	ua     string
	uaOnce sync.Once

	// throttleHits counts ChatGPT's 429 refusals on the page's own
	// /backend-api/ traffic since the last TakeThrottleHits call. It is an
	// atomic rather than mutex-guarded because countThrottled runs on
	// chromedp's event goroutine, and an atomic counter is all that goroutine
	// needs to update safely.
	throttleHits atomic.Int64
}

// countThrottled records ChatGPT throttling ANY of the page's backend calls.
//
// The SPA's "too many requests" modal is driven by these 429s, and the
// conversation capture itself can succeed (HTTP 200) while sibling calls
// (the conversation list, /backend-api/conversation/init, /textdocs, ...)
// are being refused — so this is the only signal that sees the throttling
// the user actually sees in the browser window.
//
// Only 429 counts: a 403 on some unrelated endpoint is a permissions
// answer, not a rate-limit one, and must not be conflated with throttling.
func (s *Session) countThrottled(ev any) {
	e, ok := ev.(*network.EventResponseReceived)
	if !ok || e.Response == nil {
		return
	}
	if e.Response.Status == 429 && strings.Contains(e.Response.URL, "/backend-api/") {
		s.throttleHits.Add(1)
	}
}

// TakeThrottleHits returns how many 429s countThrottled has recorded since
// the last call, resetting the count to zero in the same step (via Swap) so
// each caller observes each hit exactly once.
func (s *Session) TakeThrottleHits() int {
	return int(s.throttleHits.Swap(0))
}

// Context returns the chromedp target context of the session's page. It is
// the context to pass to chromedp.Run.
func (s *Session) Context() context.Context { return s.ctx }

// Close shuts the browser down, releasing the profile lock.
func (s *Session) Close() {
	for i := len(s.cancels) - 1; i >= 0; i-- {
		s.cancels[i]()
	}
	s.cancels = nil
}

// Launch starts the user's real browser, headed, against the persistent
// profile and returns a Session bound to its first page.
//
// The returned Session must be closed by the caller. Cancelling ctx also
// tears the browser down.
func Launch(ctx context.Context, opts Options) (*Session, error) {
	if opts.ExecPath == "" {
		return nil, fmt.Errorf("browser: Launch requires an ExecPath (call FindBrowser first)")
	}
	if opts.ProfileDir == "" {
		return nil, fmt.Errorf("browser: Launch requires a ProfileDir")
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, allocatorOptions(opts)...)
	s := &Session{ExecPath: opts.ExecPath, ProfileDir: opts.ProfileDir}
	s.cancels = append(s.cancels, allocCancel)

	tabCtx, tabCancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(chromedpErrorf))
	s.cancels = append(s.cancels, tabCancel)
	s.ctx = tabCtx

	// The first Run boots the browser and attaches to the page. Enlarging
	// the network buffers here (chromedp already enabled the domain with
	// its defaults when attaching) applies to every later capture.
	if err := chromedp.Run(tabCtx,
		network.Enable().
			WithMaxTotalBufferSize(maxTotalBufferSize).
			WithMaxResourceBufferSize(maxResourceBufferSize),
	); err != nil {
		s.Close()
		return nil, fmt.Errorf("browser: could not start %s: %w", opts.ExecPath, err)
	}

	// Lives for the whole session, so it also sees 429s during file
	// downloads and the library sweep, not just conversation captures.
	chromedp.ListenTarget(tabCtx, s.countThrottled)

	if err := assertNotAutomated(tabCtx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// assertNotAutomated fails the launch if the browser advertises itself as
// automated through navigator.webdriver.
//
// What this check does and does not cover:
//
//   - It catches the anti-automation flag going MISSING or becoming
//     ineffective: if --disable-blink-features=AutomationControlled is
//     dropped, renamed by a future Chrome, or silently rejected, Blink
//     starts reporting navigator.webdriver === true and ChatGPT's bot
//     detection sees it on the very first request.
//   - It does NOT detect forbidden flags creeping into the allocator. With
//     --disable-blink-features=AutomationControlled present, adding
//     --enable-automation still leaves navigator.webdriver === false, so
//     this assert would happily pass on a browser that is nevertheless
//     advertising itself as test-controlled through other channels. The
//     guard against that is the unit test TestAllocatorFlagsForbidden.
//
// WHY it is a hard failure rather than a warning: a leaked webdriver flag
// can cost the user their Cloudflare standing or their account, and there is
// no way to un-send the request that gives it away.
func assertNotAutomated(ctx context.Context) error {
	var raw []byte
	if err := chromedp.Run(ctx, chromedp.Evaluate("navigator.webdriver", &raw)); err != nil {
		return fmt.Errorf("browser: could not verify navigator.webdriver: %w", err)
	}
	if !webdriverOK(raw) {
		return fmt.Errorf(
			"browser: REFUSING TO CONTINUE — navigator.webdriver is %s, meaning this "+
				"browser is advertising itself as automated. ChatGPT will flag the "+
				"session. This is an automation-flag regression: the launch flags must "+
				"not include --enable-automation or --headless, and must include "+
				"--disable-blink-features=AutomationControlled (see allocatorFlags)",
			webdriverText(raw))
	}
	return nil
}

// webdriverOK reports whether the raw JSON value of navigator.webdriver is
// acceptable: false, or undefined/null (older engines simply do not define
// the property).
func webdriverOK(raw []byte) bool {
	switch webdriverText(raw) {
	case "false", "null", "undefined", "":
		return true
	}
	return false
}

// webdriverText normalises the raw evaluation result for comparison and for
// the error message.
func webdriverText(raw []byte) string {
	if len(raw) == 0 {
		return "undefined"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	if v == nil {
		return "null"
	}
	return fmt.Sprintf("%v", v)
}
