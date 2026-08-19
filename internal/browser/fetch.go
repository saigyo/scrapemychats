package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/saigyo/scrapemychats/internal/fsutil"
)

// throttleWait is the fixed pause after a 429 while listing, matching
// time.sleep(60) in export_chats.py:144.
const throttleWait = 60 * time.Second

// Fetcher is the small surface APIGet needs: a single HTTP GET performed
// inside the page. *Session implements it; tests substitute a fake so the
// throttling loop can be exercised without a browser.
type Fetcher interface {
	Fetch(apiURL string, headers map[string]string) (status int, body string, err error)
}

// fetchResult is the {status, body} object returned by fetchExpr. body is a
// pointer because the JS sets it to null when the response body could not be
// read (export_chats.py:118-120).
type fetchResult struct {
	Status int     `json:"status"`
	Body   *string `json:"body"`
}

// fetchExpr builds the in-page expression for a session-authenticated GET.
//
// It is the JS from fetch_with_session (export_chats.py:113-123), with the
// {url, headers} argument that Playwright passed separately inlined as a
// JSON literal. encoding/json escapes <, >, &, U+2028 and U+2029, so the
// literal is always valid JavaScript.
func fetchExpr(apiURL string, headers map[string]string) (string, error) {
	if headers == nil {
		// Destructuring null would throw; an empty object is what Python
		// effectively passed when auth headers were missing.
		headers = map[string]string{}
	}
	arg, err := json.Marshal(struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}{apiURL, headers})
	if err != nil {
		return "", fmt.Errorf("browser: encoding fetch arguments: %w", err)
	}
	return `(async ({url, headers}) => {
            const r = await fetch(url, {headers});
            let body = null;
            try { body = await r.text(); } catch (e) {}
            return {status: r.status, body};
        })(` + string(arg) + `)`, nil
}

// parseFetchResult decodes the raw JSON value produced by fetchExpr.
func parseFetchResult(raw []byte) (status int, body string, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, "", fmt.Errorf("browser: in-page fetch returned no result")
	}
	var res fetchResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, "", fmt.Errorf("browser: unparseable in-page fetch result %q: %w", raw, err)
	}
	if res.Body == nil {
		return res.Status, "", nil
	}
	return res.Status, *res.Body, nil
}

// FetchWithSession performs a GET from inside the page, so cookies, TLS
// fingerprint and headers match the real session. Port of
// fetch_with_session (export_chats.py:113-123). It is bounded only by the
// session context; callers that need a per-call deadline use
// fetchWithSessionCtx.
func FetchWithSession(s *Session, apiURL string, headers map[string]string) (int, string, error) {
	return fetchWithSessionCtx(s.ctx, apiURL, headers)
}

// fetchWithSessionCtx is FetchWithSession run against an explicit context, so a
// caller can bound the in-page fetch with a deadline (e.g. the eviction
// re-fetch in capture.go). ctx must derive from the session's chromedp context.
func fetchWithSessionCtx(ctx context.Context, apiURL string, headers map[string]string) (int, string, error) {
	expr, err := fetchExpr(apiURL, headers)
	if err != nil {
		return 0, "", err
	}
	var raw []byte
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &raw, awaitPromise)); err != nil {
		return 0, "", fmt.Errorf("browser: fetching %s in page: %w", apiURL, err)
	}
	return parseFetchResult(raw)
}

// Fetch makes *Session satisfy Fetcher.
func (s *Session) Fetch(apiURL string, headers map[string]string) (int, string, error) {
	return FetchWithSession(s, apiURL, headers)
}

// APIGet is FetchWithSession with the automatic 60-second wait on
// throttling. Port of api_get (export_chats.py:138-146): a 429 is retried
// forever, anything else (including an error) is returned to the caller.
func APIGet(f Fetcher, api string, headers map[string]string) (int, string, error) {
	for {
		status, body, err := f.Fetch(api, headers)
		if err != nil {
			return status, body, err
		}
		if status == 429 {
			fsutil.Log("  throttled while listing, waiting 60s...")
			sleep(throttleWait)
			continue
		}
		return status, body, nil
	}
}
