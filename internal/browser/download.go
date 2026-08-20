package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// downloadUserAgent is the FALLBACK User-Agent for out-of-page binary GETs,
// used only when the caller supplies no non-empty UA of its own — normally
// the session's real navigator.userAgent rides along with the session
// cookies (see Session.UserAgent and files.sessionClient.GetBinary), because
// ChatGPT's file hosts session-authenticate downloads and a UA that doesn't
// match the cookies' owner is one more anomaly to trip on. This constant
// merely keeps a bare fetch looking like an ordinary desktop Chrome when the
// live UA lookup fails.
const downloadUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// DefaultDownloadClient returns the *http.Client GetBinary uses when the
// caller passes nil. It follows redirects (pre-signed URLs frequently 302 to
// a storage host) and caps a single download so a stuck connection cannot
// hang the whole export.
func DefaultDownloadClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

// GetBinary performs an out-of-page GET of a download URL and returns the
// raw bytes, HTTP status and Content-Type. Port of Python's
// context.request.get(dl_url) (export_chats.py:427, 593).
//
// Playwright's context.request carries the browser context's COOKIES on such
// requests, and ChatGPT's file hosts actually require them — despite the
// download URLs being pre-signed, a cookie-less fetch is rejected with 403
// (observed live 2026-08: every attachment download failed until the session
// cookies were attached). Callers therefore pass the session's cookies (and
// its real User-Agent) via headers; see files.sessionClient.GetBinary. A nil
// or UA-less headers map falls back to a realistic desktop-Chrome User-Agent.
// The *http.Client is injectable so tests can point it at an
// httptest.Server; nil selects DefaultDownloadClient.
func GetBinary(client *http.Client, rawURL string, headers map[string]string) (status int, body []byte, contentType string, err error) {
	if client == nil {
		client = DefaultDownloadClient()
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, "", fmt.Errorf("browser: building download request for %s: %w", rawURL, err)
	}
	// Empty values are skipped so a caller whose UA/cookie lookup came up
	// empty cannot blank out a header; the default UA then still applies.
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", downloadUserAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, "", fmt.Errorf("browser: downloading %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, resp.Header.Get("Content-Type"),
			fmt.Errorf("browser: reading download body of %s: %w", rawURL, err)
	}
	return resp.StatusCode, data, resp.Header.Get("Content-Type"), nil
}

// postExpr builds the in-page expression for a session-authenticated POST of
// a JSON body. It is the JS from fetch_library (export_chats.py:476-483) with
// the {headers, body} argument Playwright passed separately inlined as a JSON
// literal, plus the request URL (which fetch_library hard-coded) carried in
// the same argument so this stays a general POST primitive.
//
// The Content-Type header is spread in AFTER the caller's headers exactly as
// Python does, so an explicit Content-Type in headers cannot override the
// application/json the endpoint expects. body is embedded as parsed JSON and
// re-serialized in the page by JSON.stringify(body), matching the Python.
func postExpr(apiURL string, headers map[string]string, body any) (string, error) {
	if headers == nil {
		headers = map[string]string{}
	}
	arg, err := json.Marshal(struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    any               `json:"body"`
	}{apiURL, headers, body})
	if err != nil {
		return "", fmt.Errorf("browser: encoding POST arguments: %w", err)
	}
	return `(async ({url, headers, body}) => {
            const r = await fetch(url, {
                method: 'POST',
                headers: {...headers, 'Content-Type': 'application/json'},
                body: JSON.stringify(body)});
            return {status: r.status, body: await r.text()};
        })(` + string(arg) + `)`, nil
}

// PostJSON performs a POST with a JSON body from inside the page, so cookies,
// TLS fingerprint and headers match the real session. Port of the in-page
// fetch in fetch_library (export_chats.py:476-483). The {status, body}
// result shares the shape FetchWithSession returns, so parseFetchResult
// decodes it.
func PostJSON(s *Session, apiURL string, headers map[string]string, body any) (int, string, error) {
	expr, err := postExpr(apiURL, headers, body)
	if err != nil {
		return 0, "", err
	}
	var raw []byte
	if err := chromedp.Run(s.ctx, chromedp.Evaluate(expr, &raw, awaitPromise)); err != nil {
		return 0, "", fmt.Errorf("browser: POSTing %s in page: %w", apiURL, err)
	}
	return parseFetchResult(raw)
}

// binExpr builds the in-page expression that fetches a URL and returns its
// body base64-encoded, so arbitrary binary bytes survive the JSON round trip
// out of the page. The bytes are chunked through String.fromCharCode.apply
// (a single apply of a multi-megabyte array overflows the JS argument stack).
// credentials:'include' sends cookies on cross-origin fetches too — fetch's
// default same-origin mode would strip them for a cookie-gated file host,
// which is the very case this fallback exists for.
func binExpr(apiURL string, headers map[string]string) (string, error) {
	if headers == nil {
		headers = map[string]string{}
	}
	arg, err := json.Marshal(struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}{apiURL, headers})
	if err != nil {
		return "", fmt.Errorf("browser: encoding binary-fetch arguments: %w", err)
	}
	return `(async ({url, headers}) => {
            const r = await fetch(url, {headers, credentials: 'include'});
            const bytes = new Uint8Array(await r.arrayBuffer());
            let bin = '';
            const chunk = 0x8000;
            for (let i = 0; i < bytes.length; i += chunk) {
                bin += String.fromCharCode.apply(null, bytes.subarray(i, i + chunk));
            }
            return {status: r.status, body: btoa(bin),
                    contentType: r.headers.get('content-type') || ''};
        })(` + string(arg) + `)`, nil
}

// binaryResult is the {status, body, contentType} object binExpr returns;
// body is the base64 text of the response bytes (nil when the fetch produced
// no body).
type binaryResult struct {
	Status      int     `json:"status"`
	Body        *string `json:"body"`
	ContentType string  `json:"contentType"`
}

// parseBinaryResult decodes the raw JSON value produced by binExpr, base64-
// decoding the body back into the original bytes.
func parseBinaryResult(raw []byte) (status int, body []byte, contentType string, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil, "", fmt.Errorf("browser: in-page binary fetch returned no result")
	}
	var res binaryResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, nil, "", fmt.Errorf("browser: unparseable in-page binary result %q: %w", raw, err)
	}
	if res.Body == nil {
		return res.Status, nil, res.ContentType, nil
	}
	data, err := base64.StdEncoding.DecodeString(*res.Body)
	if err != nil {
		return res.Status, nil, res.ContentType, fmt.Errorf("browser: decoding in-page binary body: %w", err)
	}
	return res.Status, data, res.ContentType, nil
}

// GetBinaryInPage fetches a URL from inside the page and returns the raw
// bytes and Content-Type. It is the 403 fallback for GetBinary: a URL whose
// host rejects even the cookie-carrying out-of-page GET (e.g. TLS-fingerprint
// checks) can still be reachable through the real browser's network stack,
// which an in-page fetch uses. Cross-origin file hosts must grant CORS for
// this to work; same-origin backend routes always do.
func GetBinaryInPage(s *Session, apiURL string, headers map[string]string) (int, []byte, string, error) {
	expr, err := binExpr(apiURL, headers)
	if err != nil {
		return 0, nil, "", err
	}
	var raw []byte
	if err := chromedp.Run(s.ctx, chromedp.Evaluate(expr, &raw, awaitPromise)); err != nil {
		return 0, nil, "", fmt.Errorf("browser: in-page binary fetch of %s: %w", apiURL, err)
	}
	return parseBinaryResult(raw)
}

// newDownloadClient returns a DefaultDownloadClient whose redirect handling
// re-selects the Cookie header per hop via lookup. net/http's own redirect
// logic merely copies the original Cookie header to same-domain hops (even
// when a cookie is host-only or path-scoped) and strips it on cross-domain
// ones — both over- and under-sending compared to a real browser jar, which
// re-evaluates the applicable cookies for every URL. A nil-error empty
// lookup result sends no Cookie header at all on that hop.
func newDownloadClient(lookup func(rawURL string) (string, error)) *http.Client {
	c := DefaultDownloadClient()
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// A custom CheckRedirect replaces net/http's built-in hop cap, so
		// re-impose the standard limit of 10.
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		req.Header.Del("Cookie")
		if lookup != nil {
			if ck, err := lookup(req.URL.String()); err == nil && ck != "" {
				req.Header.Set("Cookie", ck)
			}
		}
		return nil
	}
	return c
}

// DownloadClient returns the *http.Client the session's out-of-page
// downloads should use: DefaultDownloadClient plus per-redirect Cookie
// re-selection from the live browser jar (see newDownloadClient), so a
// pre-signed URL that 302s to a storage host gets exactly the cookies the
// browser would send there.
func (s *Session) DownloadClient() *http.Client {
	return newDownloadClient(s.CookieHeaderFor)
}

// CookieHeaderFor returns the Cookie header value ("name=value; ...") the
// browser would send to rawURL, read live from the session via CDP so
// rotating tokens (Cloudflare clearance, session refresh) are always
// current. An empty string with nil error means the browser has no cookies
// for that URL.
func (s *Session) CookieHeaderFor(rawURL string) (string, error) {
	var cookies []*network.Cookie
	err := chromedp.Run(s.ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var e error
		cookies, e = network.GetCookies().WithURLs([]string{rawURL}).Do(ctx)
		return e
	}))
	if err != nil {
		return "", fmt.Errorf("browser: reading cookies for %s: %w", rawURL, err)
	}
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; "), nil
}

// UserAgent returns the browser's real User-Agent string (cached after the
// first call), so out-of-page downloads present exactly the identity the
// cookies were issued to. An empty string means the lookup failed; callers
// then fall back to GetBinary's built-in desktop-Chrome UA.
func (s *Session) UserAgent() string {
	s.uaOnce.Do(func() {
		_ = chromedp.Run(s.ctx, chromedp.Evaluate("navigator.userAgent", &s.ua))
	})
	return s.ua
}
