package browser

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFetchExprEmbedsArgumentsAsJSON(t *testing.T) {
	expr, err := fetchExpr("https://chatgpt.com/backend-api/x?a=1&b=2",
		map[string]string{"Authorization": "Bearer tok", "chatgpt-account-id": "acct"})
	if err != nil {
		t.Fatalf("fetchExpr: %v", err)
	}
	// The JS body must stay identical to the Python original.
	for _, want := range []string{
		"await fetch(url, {headers})",
		"body = await r.text()",
		"return {status: r.status, body};",
	} {
		if !strings.Contains(expr, want) {
			t.Errorf("expression is missing %q:\n%s", want, expr)
		}
	}

	arg := expr[strings.LastIndex(expr, "})(")+len("})("):]
	arg = strings.TrimSuffix(arg, ")")
	var got struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal([]byte(arg), &got); err != nil {
		t.Fatalf("inlined argument is not valid JSON (%v): %s", err, arg)
	}
	if got.URL != "https://chatgpt.com/backend-api/x?a=1&b=2" {
		t.Errorf("url = %q", got.URL)
	}
	if got.Headers["Authorization"] != "Bearer tok" || got.Headers["chatgpt-account-id"] != "acct" {
		t.Errorf("headers = %v", got.Headers)
	}
}

// Header values come from the page and end up inside a JavaScript source
// string, so the encoding has to survive quotes, backslashes, angle brackets
// and the line separators that are legal in JSON but not in JS string
// literals (U+2028/U+2029).
func TestFetchExprEscapesDangerousValues(t *testing.T) {
	nasty := "a\"b\\c</script>\u2028\u2029\n"
	expr, err := fetchExpr("https://x/"+nasty, map[string]string{"X": nasty})
	if err != nil {
		t.Fatalf("fetchExpr: %v", err)
	}
	for _, bad := range []string{"\u2028", "\u2029", "\n\"", "</script>"} {
		if strings.Contains(expr[strings.Index(expr, "})("):], bad) {
			t.Errorf("inlined argument contains unescaped %q", bad)
		}
	}
}

func TestFetchExprNilHeaders(t *testing.T) {
	expr, err := fetchExpr("https://x/", nil)
	if err != nil {
		t.Fatalf("fetchExpr: %v", err)
	}
	// null would make the `({url, headers})` destructuring throw.
	if strings.Contains(expr, `"headers":null`) {
		t.Errorf("nil headers must be encoded as an empty object:\n%s", expr)
	}
	if !strings.Contains(expr, `"headers":{}`) {
		t.Errorf("expected an empty headers object:\n%s", expr)
	}
}

func TestParseFetchResult(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantStatus int
		wantBody   string
		wantErr    bool
	}{
		{name: "ok", raw: `{"status":200,"body":"{\"a\":1}"}`, wantStatus: 200, wantBody: `{"a":1}`},
		{name: "throttled", raw: `{"status":429,"body":"too many"}`, wantStatus: 429, wantBody: "too many"},
		{name: "null body", raw: `{"status":204,"body":null}`, wantStatus: 204, wantBody: ""},
		{name: "empty body", raw: `{"status":500,"body":""}`, wantStatus: 500, wantBody: ""},
		{name: "null result", raw: `null`, wantErr: true},
		{name: "no result", raw: ``, wantErr: true},
		{name: "garbage", raw: `{`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body, err := parseFetchResult([]byte(tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got status=%d body=%q", status, body)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFetchResult: %v", err)
			}
			if status != tt.wantStatus || body != tt.wantBody {
				t.Errorf("got (%d, %q), want (%d, %q)", status, body, tt.wantStatus, tt.wantBody)
			}
		})
	}
}

// fakeFetcher is a scripted Fetcher, standing in for a real page.
type fakeFetcher struct {
	replies []struct {
		status int
		body   string
		err    error
	}
	calls   int
	lastURL string
	lastHdr map[string]string
}

func (f *fakeFetcher) Fetch(apiURL string, headers map[string]string) (int, string, error) {
	f.lastURL, f.lastHdr = apiURL, headers
	i := f.calls
	f.calls++
	if i >= len(f.replies) {
		return 0, "", errors.New("fakeFetcher: unexpected extra call")
	}
	r := f.replies[i]
	return r.status, r.body, r.err
}

// withFakeSleep replaces the package sleep function for one test and returns
// the recorded durations.
func withFakeSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	orig := sleep
	sleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { sleep = orig })
	return &slept
}

func TestAPIGetRetriesOn429(t *testing.T) {
	slept := withFakeSleep(t)
	f := &fakeFetcher{replies: []struct {
		status int
		body   string
		err    error
	}{
		{status: 429, body: "slow down"},
		{status: 429, body: "slow down"},
		{status: 200, body: `{"items":[]}`},
	}}

	status, body, err := APIGet(f, "https://chatgpt.com/backend-api/conversations?offset=0",
		map[string]string{"Authorization": "Bearer tok"})
	if err != nil {
		t.Fatalf("APIGet: %v", err)
	}
	if status != 200 || body != `{"items":[]}` {
		t.Errorf("got (%d, %q), want (200, items)", status, body)
	}
	if f.calls != 3 {
		t.Errorf("fetched %d times, want 3", f.calls)
	}
	if len(*slept) != 2 {
		t.Fatalf("slept %d times, want 2", len(*slept))
	}
	for _, d := range *slept {
		if d != 60*time.Second {
			t.Errorf("throttle wait = %v, want 60s (export_chats.py:144)", d)
		}
	}
}

func TestAPIGetPassesThroughNon429(t *testing.T) {
	slept := withFakeSleep(t)
	for _, status := range []int{200, 403, 404, 500} {
		f := &fakeFetcher{replies: []struct {
			status int
			body   string
			err    error
		}{{status: status, body: "x"}}}
		got, _, err := APIGet(f, "https://x", nil)
		if err != nil {
			t.Fatalf("APIGet(%d): %v", status, err)
		}
		if got != status {
			t.Errorf("APIGet returned %d, want %d", got, status)
		}
	}
	if len(*slept) != 0 {
		t.Errorf("APIGet slept for a non-429 response")
	}
}

func TestAPIGetReturnsFetchError(t *testing.T) {
	withFakeSleep(t)
	boom := errors.New("target closed")
	f := &fakeFetcher{replies: []struct {
		status int
		body   string
		err    error
	}{{err: boom}}}
	if _, _, err := APIGet(f, "https://x", nil); !errors.Is(err, boom) {
		t.Errorf("APIGet error = %v, want %v", err, boom)
	}
}

func TestAPIGetForwardsURLAndHeaders(t *testing.T) {
	withFakeSleep(t)
	f := &fakeFetcher{replies: []struct {
		status int
		body   string
		err    error
	}{{status: 200}}}
	hdr := map[string]string{"Authorization": "Bearer tok"}
	if _, _, err := APIGet(f, "https://chatgpt.com/backend-api/files/x/download", hdr); err != nil {
		t.Fatal(err)
	}
	if f.lastURL != "https://chatgpt.com/backend-api/files/x/download" {
		t.Errorf("url = %q", f.lastURL)
	}
	if f.lastHdr["Authorization"] != "Bearer tok" {
		t.Errorf("headers = %v", f.lastHdr)
	}
}

// *Session must satisfy Fetcher, so callers can pass it straight to APIGet.
var _ Fetcher = (*Session)(nil)
