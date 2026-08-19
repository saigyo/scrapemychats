package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/saigyo/scrapemychats/internal/fsutil"
)

// sessionStateJS is the in-page expression used to read the signed-in state,
// byte-for-byte the one in export_chats.py:75-77. It resolves to null rather
// than rejecting, so a logged-out page is not an error.
const sessionStateJS = `fetch('/api/auth/session').then(r => r.json()).catch(() => null)`

// awaitPromise makes chromedp wait for the evaluated promise and return its
// resolved value, matching what Playwright's page.evaluate does implicitly.
func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}

// loginPollInterval is how often ensure-logged-in re-checks, matching
// time.sleep(5) in export_chats.py:96.
const loginPollInterval = 5 * time.Second

// SessionState returns the parsed /api/auth/session JSON for the page, or
// nil when the endpoint returned null/undefined (i.e. not signed in).
//
// Port of session_state() (export_chats.py:72-79). Unlike the Python, an
// evaluation failure is reported as an error instead of being swallowed;
// EnsureLoggedIn treats it the same way Python did (keep waiting).
func SessionState(s *Session) (map[string]any, error) {
	var raw []byte
	if err := chromedp.Run(s.ctx,
		chromedp.Evaluate(sessionStateJS, &raw, awaitPromise),
	); err != nil {
		return nil, fmt.Errorf("browser: reading /api/auth/session: %w", err)
	}
	return parseSessionState(raw)
}

// parseSessionState decodes the raw JSON value returned by sessionStateJS.
// A JSON null (or an empty result, meaning undefined) yields a nil map with
// no error, mirroring Python's `state and state.get("user")` check.
func parseSessionState(raw []byte) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("browser: /api/auth/session returned unparseable JSON: %w", err)
	}
	return state, nil
}

// stateEmail returns the signed-in user's email and whether the session
// carries a user object. The email falls back to "?" exactly as
// export_chats.py:86 does.
//
// An EMPTY user object counts as not logged in: Python's test is
// `state.get("user")`, and an empty dict is falsy there, so the Python keeps
// polling. Treating it as a login would start an export with no credentials.
func stateEmail(state map[string]any) (email string, loggedIn bool) {
	if state == nil {
		return "", false
	}
	user, ok := state["user"].(map[string]any)
	if !ok || len(user) == 0 {
		return "", false
	}
	if e, ok := user["email"].(string); ok && e != "" {
		return e, true
	}
	return "?", true
}

// loginBanner is the message printed when the user is not signed in,
// character-for-character the one in export_chats.py:88-94 (the rule is 64
// '=' characters).
var loginBanner = []string{
	"",
	strings.Repeat("=", 64),
	"Not logged in. Please log into ChatGPT in the Chrome window that",
	"just opened. If your account has multiple workspaces, switch to",
	"the workspace whose chats you want to export. The export starts",
	"automatically once the login completes (checked every 5 seconds).",
	strings.Repeat("=", 64),
}

// EnsureLoggedIn opens ChatGPT and blocks until a signed-in session exists,
// printing the same instructions as export_chats.py:82-100 and re-checking
// every 5 seconds. It returns early with an error only if the session's
// context is cancelled or the initial navigation fails.
func EnsureLoggedIn(s *Session) error {
	if err := navigate(s.ctx, BaseURL, NavTimeout); err != nil {
		return fmt.Errorf("browser: opening %s: %w", BaseURL, err)
	}
	// A failure to read the session is treated as "not logged in yet",
	// matching the bare except in session_state().
	state, _ := SessionState(s)
	if email, ok := stateEmail(state); ok {
		fsutil.Log("Logged in as " + email)
		return nil
	}
	for _, line := range loginBanner {
		fsutil.Log(line)
	}
	for {
		sleep(loginPollInterval)
		if err := s.ctx.Err(); err != nil {
			return err
		}
		state, _ := SessionState(s)
		if email, ok := stateEmail(state); ok {
			fsutil.Log("Login detected: " + email)
			return nil
		}
	}
}

// Navigate drives the page to urlstr and returns once the document has
// fired DOMContentLoaded, matching Playwright's
// page.goto(url, wait_until="domcontentloaded").
func Navigate(s *Session, urlstr string) error {
	return navigate(s.ctx, urlstr, NavTimeout)
}

// navigate issues Page.navigate and waits for the DOMContentLoaded lifecycle
// event of the loader that navigation started.
//
// chromedp.Navigate is deliberately not used: it waits for the full load
// event, which on a heavy SPA like ChatGPT can be much later than
// DOMContentLoaded (or never), whereas the Python code only ever waited for
// DOMContentLoaded.
func navigate(ctx context.Context, urlstr string, timeout time.Duration) error {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	w := newLoadWatcher()
	lctx, lcancel := context.WithCancel(runCtx)
	defer lcancel()
	chromedp.ListenTarget(lctx, w.handle)

	loaderID, err := runNavigate(runCtx, urlstr)
	if err != nil {
		return err
	}
	if loaderID == "" {
		// Same-document navigation: no new load, nothing to wait for.
		return nil
	}
	if w.watch(loaderID) {
		return nil
	}
	select {
	case <-w.finished():
		return nil
	case <-runCtx.Done():
		return fmt.Errorf("timed out waiting for DOMContentLoaded on %s: %w", urlstr, runCtx.Err())
	}
}

// runNavigate issues Page.navigate and returns the loader id of the
// navigation it started.
func runNavigate(ctx context.Context, urlstr string) (cdp.LoaderID, error) {
	var loaderID cdp.LoaderID
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		_, id, errorText, _, err := page.Navigate(urlstr).Do(c)
		if err != nil {
			return err
		}
		if errorText != "" {
			return fmt.Errorf("page load error %s", errorText)
		}
		loaderID = id
		return nil
	}))
	return loaderID, err
}

// loadWatcher waits for the DOMContentLoaded lifecycle event of a specific
// loader. The event can arrive before Page.navigate's reply tells us which
// loader to watch, so events seen before then are remembered.
//
// handle runs on chromedp's event goroutine; every field is guarded by mu.
type loadWatcher struct {
	mu       sync.Mutex
	loaderID cdp.LoaderID
	early    map[cdp.LoaderID]bool
	done     chan struct{}
	once     sync.Once
}

func newLoadWatcher() *loadWatcher {
	return &loadWatcher{early: make(map[cdp.LoaderID]bool), done: make(chan struct{})}
}

func (w *loadWatcher) handle(ev any) {
	e, ok := ev.(*page.EventLifecycleEvent)
	if !ok || e.Name != "DOMContentLoaded" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.loaderID == "" {
		w.early[e.LoaderID] = true
		return
	}
	if e.LoaderID == w.loaderID {
		w.finish()
	}
}

// watch starts watching id and reports whether its DOMContentLoaded already
// arrived.
func (w *loadWatcher) watch(id cdp.LoaderID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.loaderID = id
	if w.early[id] {
		w.finish()
		return true
	}
	return false
}

// finish must be called with mu held.
func (w *loadWatcher) finish() { w.once.Do(func() { close(w.done) }) }

func (w *loadWatcher) finished() <-chan struct{} { return w.done }
