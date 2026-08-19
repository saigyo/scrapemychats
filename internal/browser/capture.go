package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// ErrCaptureTimeout is returned when the expected backend response never
// arrived within NavTimeout. It is the equivalent of Playwright's
// TimeoutError, which export_chats.py:788-792 catches to retry the chat and
// hint at a Cloudflare challenge, so callers should test for it with
// errors.Is.
var ErrCaptureTimeout = errors.New("browser: timed out waiting for the conversation response")

// authHeaderNames are the request headers ChatGPT's own frontend sends that
// we reuse for direct API calls, mapped to the exact casing the Python code
// put in the outgoing header dict (export_chats.py:103-110).
var authHeaderNames = []struct{ in, out string }{
	{"authorization", "Authorization"},
	{"chatgpt-account-id", "chatgpt-account-id"},
}

// AuthHeaders extracts the auth headers ChatGPT's own frontend uses. Port of
// auth_headers_from (export_chats.py:103-110).
//
// The lookup is case-insensitive because CDP reports header names as the
// network stack saw them: HTTP/2 lowercases everything, but
// Network.requestWillBeSent can also report the casing the page used. The
// returned map always uses the canonical casing above.
func AuthHeaders(reqHeaders map[string]string) map[string]string {
	auth := map[string]string{}
	for _, h := range authHeaderNames {
		if v := headerLookup(reqHeaders, h.in); v != "" {
			auth[h.out] = v
		}
	}
	return auth
}

// headerLookup finds name in headers ignoring case. An exact match wins;
// otherwise keys are scanned in sorted order so the result is deterministic
// when a (malformed) header set contains several spellings.
func headerLookup(headers map[string]string, name string) string {
	if v, ok := headers[name]; ok {
		return v
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.EqualFold(k, name) && headers[k] != "" {
			return headers[k]
		}
	}
	return ""
}

// headerMap converts CDP's map[string]any header representation to strings.
func headerMap(h network.Headers) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		switch v := v.(type) {
		case string:
			out[k] = v
		case nil:
		default:
			out[k] = fmt.Sprintf("%v", v)
		}
	}
	return out
}

// mergeHeaders copies src over dst (src wins), creating dst if needed.
func mergeHeaders(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = make(map[string]string, len(src))
	}
	for k, v := range src {
		if v != "" {
			dst[k] = v
		}
	}
	return dst
}

// ---------------------------------------------------------------- auth grab

// authWatcher collects the request headers of the frontend's own
// conversation-list request. Its handle method runs on chromedp's event
// goroutine, so every field is guarded by mu and the result is handed over
// through a buffered channel.
type authWatcher struct {
	needle string

	mu       sync.Mutex
	reqs     map[network.RequestID]map[string]string
	sawMatch bool // a matching request was seen, with or without auth headers

	once sync.Once
	ch   chan map[string]string
}

func newAuthWatcher(needle string) *authWatcher {
	return &authWatcher{
		needle: needle,
		reqs:   make(map[network.RequestID]map[string]string),
		ch:     make(chan map[string]string, 1),
	}
}

func (w *authWatcher) handle(ev any) {
	switch e := ev.(type) {
	case *network.EventRequestWillBeSent:
		if e.Request == nil || !strings.Contains(e.Request.URL, w.needle) {
			return
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		h := mergeHeaders(w.reqs[e.RequestID], headerMap(e.Request.Headers))
		w.reqs[e.RequestID] = h
		w.consider(h)
	case *network.EventRequestWillBeSentExtraInfo:
		// Wire headers for a request we already matched by URL; they are
		// what actually went out, so they win over the provisional ones.
		w.mu.Lock()
		defer w.mu.Unlock()
		h, ok := w.reqs[e.RequestID]
		if !ok {
			return
		}
		h = mergeHeaders(h, headerMap(e.Headers))
		w.reqs[e.RequestID] = h
		w.consider(h)
	}
}

// consider must be called with mu held. It notes that a matching request was
// seen (used only to explain a timeout) and completes the wait as soon as a
// request carries usable auth headers.
func (w *authWatcher) consider(h map[string]string) {
	w.sawMatch = true
	auth := AuthHeaders(h)
	if len(auth) == 0 {
		return
	}
	w.once.Do(func() { w.ch <- auth })
}

// CaptureAuth loads the homepage and copies the auth headers the frontend
// itself uses. Port of capture_auth (export_chats.py:129-135).
//
// Playwright waited for the *response* and then read response.request
// headers; CDP reports the request headers earlier and separately, so this
// resolves as soon as a matching request carries an Authorization header.
// That is strictly earlier and cannot observe fewer headers.
//
// It fails rather than returning empty headers: Python raised TimeoutError
// here, and handing an empty auth map to the callers would turn one failed
// capture into a run's worth of 401s.
func CaptureAuth(s *Session) (map[string]string, error) {
	w := newAuthWatcher("/backend-api/conversations?")
	lctx, lcancel := context.WithCancel(s.ctx)
	defer lcancel()
	chromedp.ListenTarget(lctx, w.handle)

	navErrCh := navigateAsync(s.ctx, BaseURL)
	timer := time.NewTimer(NavTimeout)
	defer timer.Stop()

	var navErr error
	for {
		select {
		case auth := <-w.ch:
			return auth, nil
		case err := <-navErrCh:
			// A navigation error is only fatal if no matching request
			// shows up either; ChatGPT's SPA aborts navigations routinely.
			navErr = err
			navErrCh = nil
		case <-timer.C:
			w.mu.Lock()
			sawMatch := w.sawMatch
			w.mu.Unlock()
			return nil, authTimeoutError(sawMatch, navErr)
		case <-s.ctx.Done():
			return nil, s.ctx.Err()
		}
	}
}

// authTimeoutError explains why CaptureAuth gave up. sawMatch distinguishes
// "the conversation-list request never happened" (usually a Cloudflare
// challenge or a lost login) from "it happened but carried no auth headers",
// which points at a change in how the frontend authenticates. Both wrap
// ErrCaptureTimeout so callers can treat them as Playwright's TimeoutError.
func authTimeoutError(sawMatch bool, navErr error) error {
	if navErr != nil {
		return navErr
	}
	if sawMatch {
		return fmt.Errorf("capturing auth headers from %s: the frontend's "+
			"conversation-list request carried no Authorization header: %w",
			BaseURL, ErrCaptureTimeout)
	}
	return fmt.Errorf("capturing auth headers from %s: the frontend never "+
		"requested its conversation list (Cloudflare check? not logged in?): %w",
		BaseURL, ErrCaptureTimeout)
}

// navigateAsync starts a navigation on another goroutine and reports its
// outcome on the returned channel. The capture watchers must keep running
// while the page loads, which is why the navigation cannot simply block the
// caller.
func navigateAsync(ctx context.Context, urlstr string) chan error {
	ch := make(chan error, 1)
	go func() { ch <- navigate(ctx, urlstr, NavTimeout) }()
	return ch
}

// -------------------------------------------------------- conversation grab

// convRequest is what we remember about an in-flight request whose URL
// matches the conversation we are capturing.
type convRequest struct {
	method  string
	headers map[string]string
}

// convWatcher matches the frontend's GET of one conversation's JSON and
// signals the two moments the caller cares about: the response headers
// (status + request headers) and the body having finished loading.
//
// handle runs on chromedp's event goroutine; mu guards every field and the
// signals are buffered channels closed exactly once.
type convWatcher struct {
	needle string

	mu      sync.Mutex
	reqs    map[network.RequestID]*convRequest
	matched network.RequestID
	hasResp bool
	status  int
	headers map[string]string

	respOnce sync.Once
	respCh   chan struct{}
	doneOnce sync.Once
	doneCh   chan struct{}
	failOnce sync.Once
	failCh   chan string
}

func newConvWatcher(needle string) *convWatcher {
	return &convWatcher{
		needle: needle,
		// Scoped to a single capture and only ever holding requests whose
		// URL contains this conversation's id, so it cannot grow unbounded.
		reqs:   make(map[network.RequestID]*convRequest),
		respCh: make(chan struct{}),
		doneCh: make(chan struct{}),
		failCh: make(chan string, 1),
	}
}

func (w *convWatcher) handle(ev any) {
	switch e := ev.(type) {
	case *network.EventRequestWillBeSent:
		if e.Request == nil || !strings.Contains(e.Request.URL, w.needle) {
			return
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		r := w.reqs[e.RequestID]
		if r == nil {
			r = &convRequest{}
			w.reqs[e.RequestID] = r
		}
		r.method = e.Request.Method
		r.headers = mergeHeaders(r.headers, headerMap(e.Request.Headers))

	case *network.EventRequestWillBeSentExtraInfo:
		w.mu.Lock()
		defer w.mu.Unlock()
		if r := w.reqs[e.RequestID]; r != nil {
			r.headers = mergeHeaders(r.headers, headerMap(e.Headers))
		}

	case *network.EventResponseReceived:
		if e.Response == nil || !strings.Contains(e.Response.URL, w.needle) {
			return
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.hasResp {
			return
		}
		// Mirrors the Python predicate: the URL must match AND the request
		// method must be GET (the frontend also POSTs to this path).
		r := w.reqs[e.RequestID]
		if r == nil || !strings.EqualFold(r.method, "GET") {
			return
		}
		w.matched = e.RequestID
		w.hasResp = true
		w.status = int(e.Response.Status)
		w.headers = mergeHeaders(headerMap(e.Response.RequestHeaders), r.headers)
		w.respOnce.Do(func() { close(w.respCh) })

	case *network.EventLoadingFinished:
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.hasResp && e.RequestID == w.matched {
			w.doneOnce.Do(func() { close(w.doneCh) })
		}

	case *network.EventLoadingFailed:
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.hasResp && e.RequestID == w.matched {
			w.failOnce.Do(func() { w.failCh <- e.ErrorText })
		}
	}
}

// result returns what the response event carried.
func (w *convWatcher) result() (network.RequestID, int, map[string]string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.matched, w.status, w.headers
}

// CaptureConversation navigates to a chat and captures the conversation JSON
// the page fetched for itself, plus the auth headers it used. Port of
// capture_conversation (export_chats.py:668-679).
//
// Returns (nil, nil, status, nil) when the backend answered with anything
// other than 200, exactly like the Python, so the caller can apply its
// rate-limit handling. A missing response within NavTimeout is reported as
// ErrCaptureTimeout.
func CaptureConversation(s *Session, urlstr, cid string) (map[string]any, map[string]string, int, error) {
	needle := "/backend-api/conversation/" + cid
	w := newConvWatcher(needle)
	lctx, lcancel := context.WithCancel(s.ctx)
	defer lcancel()
	chromedp.ListenTarget(lctx, w.handle)

	navErrCh := navigateAsync(s.ctx, urlstr)
	timer := time.NewTimer(NavTimeout)
	defer timer.Stop()

	var navErr error
wait:
	for {
		select {
		case <-w.respCh:
			break wait
		case err := <-navErrCh:
			// Not fatal on its own: the response may still be in flight.
			navErr = err
			navErrCh = nil
		case <-timer.C:
			if navErr != nil {
				return nil, nil, 0, navErr
			}
			return nil, nil, 0, fmt.Errorf("%s: %w", urlstr, ErrCaptureTimeout)
		case <-s.ctx.Done():
			return nil, nil, 0, s.ctx.Err()
		}
	}

	reqID, status, reqHeaders := w.result()
	auth := AuthHeaders(reqHeaders)
	if status != 200 {
		return nil, nil, status, nil
	}

	body, status, err := conversationBody(s, w, reqID, cid, auth, timer)
	if err != nil {
		return nil, nil, 0, err
	}
	if status != 200 {
		return nil, nil, status, nil
	}

	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, nil, 0, fmt.Errorf("browser: conversation %s returned unparseable JSON: %w", cid, err)
	}
	return data, auth, 200, nil
}

// conversationBody retrieves the captured response body.
//
// Chrome only keeps response bodies in a per-target buffer, and evicts them
// under memory pressure — Playwright had its own copy, raw CDP does not. Two
// defences, in order:
//
//  1. the buffer is enlarged at launch (see maxTotalBufferSize) and
//     Network.getResponseBody is called the instant loadingFinished fires,
//     minimising the window in which eviction can happen;
//  2. if the body is gone anyway ("No data found for resource with given
//     identifier"), or never finished loading, the conversation is re-fetched
//     with an in-page fetch() using the auth headers we just captured. That
//     request is indistinguishable from the frontend's own, so it costs one
//     extra API call and nothing else. Its status is returned as-is, so a 429
//     on the retry still reaches the caller's rate-limit handling.
func conversationBody(s *Session, w *convWatcher, reqID network.RequestID, cid string,
	auth map[string]string, timer *time.Timer,
) ([]byte, int, error) {
	select {
	case <-w.doneCh:
		body, err := responseBody(s, reqID)
		if err == nil {
			return body, 200, nil
		}
		// Evicted (or otherwise unavailable): fall through to the re-fetch.
	case <-w.failCh:
		// The transfer failed after the headers arrived; the re-fetch
		// below reports the real outcome.
	case <-timer.C:
	case <-s.ctx.Done():
		return nil, 0, s.ctx.Err()
	}

	// Bound the re-fetch: this path is reached only after a wait already
	// failed, so a stalled in-page fetch must not hang the export until Ctrl+C
	// (reported by Copilot). NavTimeout matches the main capture wait.
	fetchCtx, cancel := context.WithTimeout(s.ctx, NavTimeout)
	defer cancel()
	status, body, err := fetchWithSessionCtx(fetchCtx, BaseURL+"/backend-api/conversation/"+cid, auth)
	if err != nil {
		return nil, 0, classifyRefetchErr(cid, s.ctx.Err(), fetchCtx.Err(), err)
	}
	return []byte(body), status, nil
}

// classifyRefetchErr maps a failed bounded re-fetch to the error the export
// loop expects. A real session cancellation (Ctrl+C) propagates verbatim. Our
// own NavTimeout deadline becomes the retriable ErrCaptureTimeout — crucially
// NOT a bare context.DeadlineExceeded, which captureWithRetry treats as
// cancellation and would abort the whole export on. Anything else is wrapped
// verbatim.
func classifyRefetchErr(cid string, sessErr, fetchCtxErr, rawErr error) error {
	if sessErr != nil {
		return sessErr
	}
	if errors.Is(fetchCtxErr, context.DeadlineExceeded) {
		return fmt.Errorf("browser: re-fetching conversation %s timed out: %w", cid, ErrCaptureTimeout)
	}
	return fmt.Errorf("browser: re-fetching conversation %s after the "+
		"captured body was unavailable: %w", cid, rawErr)
}

// responseBody asks Chrome for the body of an already-received response.
func responseBody(s *Session, reqID network.RequestID) ([]byte, error) {
	var body []byte
	err := chromedp.Run(s.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		body, err = network.GetResponseBody(reqID).Do(ctx)
		return err
	}))
	return body, err
}
