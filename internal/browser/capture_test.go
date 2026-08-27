package browser

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/network"
)

func TestClassifyRefetchErr(t *testing.T) {
	// A live session cancellation (Ctrl+C) propagates verbatim.
	if got := classifyRefetchErr("cid", context.Canceled, nil, nil); !errors.Is(got, context.Canceled) {
		t.Errorf("session-cancelled: got %v, want context.Canceled", got)
	}

	// Our NavTimeout deadline must become the retriable ErrCaptureTimeout and
	// must NOT satisfy errors.Is(context.DeadlineExceeded) — otherwise the
	// export loop, which checks DeadlineExceeded first, would abort the whole
	// run instead of retrying this one chat.
	got := classifyRefetchErr("cid", nil, context.DeadlineExceeded, context.DeadlineExceeded)
	if !errors.Is(got, ErrCaptureTimeout) {
		t.Errorf("timeout: got %v, want wrapped ErrCaptureTimeout", got)
	}
	if errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("timeout error must NOT wrap context.DeadlineExceeded (loop would treat it as cancellation): %v", got)
	}

	// Any other failure is wrapped verbatim and is neither sentinel.
	other := errors.New("boom")
	got = classifyRefetchErr("cid", nil, nil, other)
	if !errors.Is(got, other) {
		t.Errorf("other error should wrap the cause: %v", got)
	}
	if errors.Is(got, ErrCaptureTimeout) || errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("other error must not look like a timeout: %v", got)
	}
}

func TestAuthHeaders(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{
			name: "lowercase as CDP reports them",
			in: map[string]string{
				"authorization":      "Bearer tok",
				"chatgpt-account-id": "acct-1",
				"user-agent":         "Mozilla/5.0",
			},
			want: map[string]string{"Authorization": "Bearer tok", "chatgpt-account-id": "acct-1"},
		},
		{
			name: "canonical casing as the page may send it",
			in: map[string]string{
				"Authorization":      "Bearer tok",
				"ChatGPT-Account-Id": "acct-1",
			},
			want: map[string]string{"Authorization": "Bearer tok", "chatgpt-account-id": "acct-1"},
		},
		{
			name: "screaming casing",
			in:   map[string]string{"AUTHORIZATION": "Bearer tok"},
			want: map[string]string{"Authorization": "Bearer tok"},
		},
		{
			name: "only the account id",
			in:   map[string]string{"chatgpt-account-id": "acct-1"},
			want: map[string]string{"chatgpt-account-id": "acct-1"},
		},
		{
			name: "empty values are dropped, like Python's truthiness check",
			in:   map[string]string{"authorization": "", "chatgpt-account-id": ""},
			want: map[string]string{},
		},
		{
			name: "nothing to extract",
			in:   map[string]string{"accept": "*/*"},
			want: map[string]string{},
		},
		{
			name: "nil input",
			in:   nil,
			want: map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AuthHeaders(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("AuthHeaders(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// AuthHeaders must never hand back the caller's map, or a later merge would
// corrupt the captured request headers.
func TestAuthHeadersDoesNotAliasInput(t *testing.T) {
	in := map[string]string{"authorization": "Bearer tok"}
	got := AuthHeaders(in)
	got["Authorization"] = "tampered"
	if in["authorization"] != "Bearer tok" {
		t.Error("AuthHeaders returned a map aliasing its input")
	}
}

func TestHeaderMap(t *testing.T) {
	got := headerMap(network.Headers{
		"authorization":  "Bearer tok",
		"content-length": float64(42), // CDP values are JSON, so numbers appear
		"nothing":        nil,
	})
	want := map[string]string{"authorization": "Bearer tok", "content-length": "42"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("headerMap = %v, want %v", got, want)
	}
}

func TestMergeHeaders(t *testing.T) {
	got := mergeHeaders(map[string]string{"a": "1", "b": "2"}, map[string]string{"b": "3", "c": "", "d": "4"})
	want := map[string]string{"a": "1", "b": "3", "d": "4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeHeaders = %v, want %v", got, want)
	}
	if got := mergeHeaders(nil, map[string]string{"a": "1"}); got["a"] != "1" {
		t.Errorf("mergeHeaders into nil = %v", got)
	}
}

const testCID = "0f8fad5b-d9cb-469f-a165-70867728950e"

func convURL(cid string) string {
	return BaseURL + "/backend-api/conversation/" + cid
}

func requestEvent(id network.RequestID, url, method string, hdr network.Headers) *network.EventRequestWillBeSent {
	return &network.EventRequestWillBeSent{
		RequestID: id,
		Request:   &network.Request{URL: url, Method: method, Headers: hdr},
	}
}

func responseEvent(id network.RequestID, url string, status int64, reqHdr network.Headers) *network.EventResponseReceived {
	return &network.EventResponseReceived{
		RequestID: id,
		Response:  &network.Response{URL: url, Status: status, RequestHeaders: reqHdr},
	}
}

func responded(w *convWatcher) bool {
	select {
	case <-w.respCh:
		return true
	default:
		return false
	}
}

func TestConvWatcherMatchesGetResponse(t *testing.T) {
	w := newConvWatcher(testCID)
	w.handle(requestEvent("1", convURL(testCID), "GET", network.Headers{
		"authorization":      "Bearer tok",
		"chatgpt-account-id": "acct-1",
	}))
	if responded(w) {
		t.Fatal("signalled before the response event")
	}
	w.handle(responseEvent("1", convURL(testCID), 200, nil))
	if !responded(w) {
		t.Fatal("did not signal after the matching response")
	}
	id, status, hdr, _ := w.result()
	if id != "1" || status != 200 {
		t.Errorf("result = (%v, %d), want (1, 200)", id, status)
	}
	if got := AuthHeaders(hdr); got["Authorization"] != "Bearer tok" || got["chatgpt-account-id"] != "acct-1" {
		t.Errorf("captured auth headers = %v", got)
	}
}

func TestConvWatcherExtraInfoBeforeRequestEvent(t *testing.T) {
	w := newConvWatcher(testCID)
	// ExtraInfo (the wire Authorization) arrives before the URL-bearing request
	// event, which the protocol permits — it must be buffered, not dropped.
	w.handle(&network.EventRequestWillBeSentExtraInfo{
		RequestID: "1",
		Headers:   network.Headers{"authorization": "Bearer wire"},
	})
	w.handle(requestEvent("1", convURL(testCID), "GET",
		network.Headers{"chatgpt-account-id": "acct-1"}))
	w.handle(responseEvent("1", convURL(testCID), 200, nil))
	if !responded(w) {
		t.Fatal("did not signal after the matching response")
	}
	_, _, hdr, _ := w.result()
	got := AuthHeaders(hdr)
	if got["Authorization"] != "Bearer wire" || got["chatgpt-account-id"] != "acct-1" {
		t.Errorf("captured auth headers = %v, want the buffered wire Authorization merged in", got)
	}
}

// export_chats.py:671-672 requires BOTH the URL match and method == GET; the
// frontend POSTs to the same path while the chat is open.
func TestConvWatcherIgnoresNonGet(t *testing.T) {
	w := newConvWatcher(testCID)
	w.handle(requestEvent("1", convURL(testCID), "POST", nil))
	w.handle(responseEvent("1", convURL(testCID), 200, nil))
	if responded(w) {
		t.Error("a POST to the conversation endpoint must not be captured")
	}
}

func TestConvWatcherIgnoresOtherConversations(t *testing.T) {
	w := newConvWatcher(testCID)
	other := "11111111-2222-3333-4444-555555555555"
	w.handle(requestEvent("1", convURL(other), "GET", nil))
	w.handle(responseEvent("1", convURL(other), 200, nil))
	if responded(w) {
		t.Error("another conversation's response must not be captured")
	}
	if len(w.reqs) != 0 {
		t.Errorf("non-matching requests must not be tracked (map has %d entries)", len(w.reqs))
	}
}

// A response with no preceding requestWillBeSent cannot have its method
// checked, so it is skipped rather than guessed at.
func TestConvWatcherIgnoresUnknownRequestID(t *testing.T) {
	w := newConvWatcher(testCID)
	w.handle(responseEvent("9", convURL(testCID), 200, nil))
	if responded(w) {
		t.Error("captured a response with no matching request")
	}
}

func TestConvWatcherNonOKStatus(t *testing.T) {
	w := newConvWatcher(testCID)
	w.handle(requestEvent("1", convURL(testCID), "GET", nil))
	w.handle(responseEvent("1", convURL(testCID), 429, nil))
	if !responded(w) {
		t.Fatal("a non-200 response must still be reported, so the caller can back off")
	}
	if _, status, _, _ := w.result(); status != 429 {
		t.Errorf("status = %d, want 429", status)
	}
}

// requestWillBeSentExtraInfo carries the headers actually put on the wire;
// the auth header is often only visible there.
func TestConvWatcherMergesExtraInfoHeaders(t *testing.T) {
	w := newConvWatcher(testCID)
	w.handle(requestEvent("1", convURL(testCID), "GET", network.Headers{"accept": "*/*"}))
	w.handle(&network.EventRequestWillBeSentExtraInfo{
		RequestID: "1",
		Headers:   network.Headers{"authorization": "Bearer wire"},
	})
	w.handle(responseEvent("1", convURL(testCID), 200, nil))
	_, _, hdr, _ := w.result()
	if AuthHeaders(hdr)["Authorization"] != "Bearer wire" {
		t.Errorf("extra-info headers were not merged: %v", hdr)
	}
}

func TestConvWatcherIgnoresExtraInfoForUnknownRequests(t *testing.T) {
	w := newConvWatcher(testCID)
	w.handle(&network.EventRequestWillBeSentExtraInfo{
		RequestID: "unrelated",
		Headers:   network.Headers{"authorization": "Bearer tok"},
	})
	if len(w.reqs) != 0 {
		t.Errorf("extra-info for an untracked request must not allocate state: %v", w.reqs)
	}
}

// Response.requestHeaders is the refined set the network stack sent; use it
// where the earlier events had nothing.
func TestConvWatcherFallsBackToResponseRequestHeaders(t *testing.T) {
	w := newConvWatcher(testCID)
	w.handle(requestEvent("1", convURL(testCID), "GET", nil))
	w.handle(responseEvent("1", convURL(testCID), 200, network.Headers{
		"authorization": "Bearer resp",
	}))
	_, _, hdr, _ := w.result()
	if AuthHeaders(hdr)["Authorization"] != "Bearer resp" {
		t.Errorf("response request-headers were not used: %v", hdr)
	}
}

func TestConvWatcherLoadingFinishedOnlyForMatchedRequest(t *testing.T) {
	w := newConvWatcher(testCID)
	w.handle(requestEvent("1", convURL(testCID), "GET", nil))
	w.handle(responseEvent("1", convURL(testCID), 200, nil))

	w.handle(&network.EventLoadingFinished{RequestID: "2"})
	select {
	case <-w.doneCh:
		t.Fatal("another request's loadingFinished completed the capture")
	default:
	}

	w.handle(&network.EventLoadingFinished{RequestID: "1"})
	select {
	case <-w.doneCh:
	default:
		t.Fatal("loadingFinished for the matched request did not complete the capture")
	}
}

func TestConvWatcherLoadingFailed(t *testing.T) {
	w := newConvWatcher(testCID)
	w.handle(requestEvent("1", convURL(testCID), "GET", nil))
	w.handle(responseEvent("1", convURL(testCID), 200, nil))
	w.handle(&network.EventLoadingFailed{RequestID: "1", ErrorText: "net::ERR_ABORTED"})
	select {
	case msg := <-w.failCh:
		if msg != "net::ERR_ABORTED" {
			t.Errorf("failure text = %q", msg)
		}
	default:
		t.Fatal("loadingFailed for the matched request was not reported")
	}
}

// Only the first matching response wins, mirroring expect_response.
func TestConvWatcherKeepsFirstResponse(t *testing.T) {
	w := newConvWatcher(testCID)
	w.handle(requestEvent("1", convURL(testCID), "GET", nil))
	w.handle(responseEvent("1", convURL(testCID), 200, nil))
	w.handle(requestEvent("2", convURL(testCID), "GET", nil))
	w.handle(responseEvent("2", convURL(testCID), 500, nil))
	if id, status, _, _ := w.result(); id != "1" || status != 200 {
		t.Errorf("result = (%v, %d), want the first response (1, 200)", id, status)
	}
}

func TestConversationDetailURL(t *testing.T) {
	const other = "11111111-2222-3333-4444-555555555555"
	tests := []struct {
		name        string
		url         string
		cid         string
		wantMatch   bool
		wantMapping bool
	}{
		{
			name:        "singular mapping-shaped endpoint",
			url:         BaseURL + "/backend-api/conversation/" + testCID,
			cid:         testCID,
			wantMatch:   true,
			wantMapping: true,
		},
		{
			name:        "plural paginated endpoint with query string",
			url:         BaseURL + "/backend-api/conversations/" + testCID + "?include_has_versions=true&num_turns=10",
			cid:         testCID,
			wantMatch:   true,
			wantMapping: false,
		},
		{
			// The regression: a sub-resource under the singular endpoint must
			// not be mistaken for the conversation itself.
			name:        "textdocs sub-resource of the singular endpoint",
			url:         BaseURL + "/backend-api/conversation/" + testCID + "/textdocs",
			cid:         testCID,
			wantMatch:   false,
			wantMapping: false,
		},
		{
			name:        "another conversation's singular endpoint",
			url:         BaseURL + "/backend-api/conversation/" + other,
			cid:         testCID,
			wantMatch:   false,
			wantMapping: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match, mappingShaped := conversationDetailURL(tt.url, tt.cid)
			if match != tt.wantMatch || mappingShaped != tt.wantMapping {
				t.Errorf("conversationDetailURL(%q, %q) = (%v, %v), want (%v, %v)",
					tt.url, tt.cid, match, mappingShaped, tt.wantMatch, tt.wantMapping)
			}
		})
	}
}

// The regression this guards against: a substring match on
// "/backend-api/conversation/<cid>" also matched /textdocs, and that
// sub-resource's non-mapping body ("[]") won the first-response-wins race.
func TestConvWatcherIgnoresTextdocsSubResource(t *testing.T) {
	w := newConvWatcher(testCID)
	textdocsURL := convURL(testCID) + "/textdocs"
	w.handle(requestEvent("1", textdocsURL, "GET", nil))
	w.handle(responseEvent("1", textdocsURL, 200, nil))
	if responded(w) {
		t.Error("a /textdocs sub-resource response must not be captured as the conversation itself")
	}
}

// The plural endpoint must still be recognised as the conversation response
// (so the caller knows the frontend answered and can move on to a targeted
// re-fetch), but flagged as not mapping-shaped.
func TestConvWatcherMatchesPluralEndpointAsNonMapping(t *testing.T) {
	w := newConvWatcher(testCID)
	pluralURL := BaseURL + "/backend-api/conversations/" + testCID + "?include_has_versions=true&num_turns=10"
	w.handle(requestEvent("1", pluralURL, "GET", nil))
	w.handle(responseEvent("1", pluralURL, 200, nil))
	if !responded(w) {
		t.Fatal("the plural conversation endpoint must still signal respCh")
	}
	_, status, _, mappingShaped := w.result()
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if mappingShaped {
		t.Error("the plural endpoint must be reported as not mapping-shaped")
	}
}

func TestAuthWatcherSignalsOnAuthHeaders(t *testing.T) {
	w := newAuthWatcher("/backend-api/conversations?")
	w.handle(requestEvent("1", BaseURL+"/backend-api/conversations?offset=0&limit=28", "GET",
		network.Headers{"authorization": "Bearer tok", "chatgpt-account-id": "acct-1"}))
	select {
	case auth := <-w.ch:
		want := map[string]string{"Authorization": "Bearer tok", "chatgpt-account-id": "acct-1"}
		if !reflect.DeepEqual(auth, want) {
			t.Errorf("auth = %v, want %v", auth, want)
		}
	default:
		t.Fatal("a request carrying auth headers did not complete the capture")
	}
}

func TestAuthWatcherIgnoresOtherURLs(t *testing.T) {
	w := newAuthWatcher("/backend-api/conversations?")
	// No "?" — the conversation *detail* endpoint, not the list.
	w.handle(requestEvent("1", BaseURL+"/backend-api/conversation/"+testCID, "GET",
		network.Headers{"authorization": "Bearer tok"}))
	select {
	case auth := <-w.ch:
		t.Fatalf("unrelated URL captured: %v", auth)
	default:
	}
}

func TestAuthWatcherWaitsForHeadersViaExtraInfo(t *testing.T) {
	w := newAuthWatcher("/backend-api/conversations?")
	w.handle(requestEvent("1", BaseURL+"/backend-api/conversations?offset=0", "GET",
		network.Headers{"accept": "*/*"}))
	select {
	case auth := <-w.ch:
		t.Fatalf("completed with no auth headers: %v", auth)
	default:
	}
	if !w.sawMatch {
		t.Error("a matching request with no auth headers should still be recorded")
	}
	w.handle(&network.EventRequestWillBeSentExtraInfo{
		RequestID: "1",
		Headers:   network.Headers{"authorization": "Bearer wire"},
	})
	select {
	case auth := <-w.ch:
		if auth["Authorization"] != "Bearer wire" {
			t.Errorf("auth = %v", auth)
		}
	default:
		t.Fatal("extra-info headers did not complete the capture")
	}
}

// A timeout must never be reported as success with an empty auth map: the
// callers would send unauthenticated requests and collect 401s for the whole
// run. Python raised TimeoutError here.
func TestAuthTimeoutError(t *testing.T) {
	tests := []struct {
		name     string
		sawMatch bool
		navErr   error
		wantHint string
	}{
		{name: "no matching request", wantHint: "never requested its conversation list"},
		{name: "request without auth", sawMatch: true, wantHint: "no Authorization header"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := authTimeoutError(tt.sawMatch, tt.navErr)
			if err == nil {
				t.Fatal("a timeout must be an error, not an empty auth map")
			}
			if !errors.Is(err, ErrCaptureTimeout) {
				t.Errorf("error %v does not wrap ErrCaptureTimeout", err)
			}
			if !strings.Contains(err.Error(), tt.wantHint) {
				t.Errorf("error %q does not explain the cause (%q)", err, tt.wantHint)
			}
		})
	}
}

// A navigation failure is the more specific diagnosis, so it wins.
func TestAuthTimeoutErrorPrefersNavError(t *testing.T) {
	boom := errors.New("page load error net::ERR_NAME_NOT_RESOLVED")
	if err := authTimeoutError(false, boom); !errors.Is(err, boom) {
		t.Errorf("authTimeoutError = %v, want the navigation error %v", err, boom)
	}
}

// The signal channel is buffered and written once, so a burst of matching
// requests can never block chromedp's event goroutine.
func TestAuthWatcherRequiresAuthorizationNotAccountIDAlone(t *testing.T) {
	w := newAuthWatcher("/backend-api/conversations?")
	// A matching request carrying only the account id (no bearer token) must
	// NOT complete the capture — publishing it would leave discovery
	// unauthenticated.
	w.handle(requestEvent("1", BaseURL+"/backend-api/conversations?offset=0", "GET",
		network.Headers{"chatgpt-account-id": "acct-1"}))
	select {
	case auth := <-w.ch:
		t.Fatalf("completed with account-id but no Authorization: %v", auth)
	default:
	}
	if !w.sawMatch {
		t.Error("the matching request should be recorded for the timeout diagnostic")
	}
	// Authorization arrives later via ExtraInfo; now it completes with BOTH.
	w.handle(&network.EventRequestWillBeSentExtraInfo{
		RequestID: "1",
		Headers:   network.Headers{"authorization": "Bearer late"},
	})
	select {
	case auth := <-w.ch:
		want := map[string]string{"Authorization": "Bearer late", "chatgpt-account-id": "acct-1"}
		if !reflect.DeepEqual(auth, want) {
			t.Errorf("auth = %v, want %v", auth, want)
		}
	default:
		t.Fatal("Authorization arriving via ExtraInfo did not complete the capture")
	}
}

func TestAuthWatcherExtraInfoBeforeRequestEvent(t *testing.T) {
	w := newAuthWatcher("/backend-api/conversations?")
	// CDP delivers the wire headers (carrying the only Authorization) FIRST,
	// before the URL-bearing request event. The ExtraInfo must be buffered, not
	// dropped.
	w.handle(&network.EventRequestWillBeSentExtraInfo{
		RequestID: "1",
		Headers:   network.Headers{"authorization": "Bearer wire"},
	})
	select {
	case auth := <-w.ch:
		t.Fatalf("completed before the request event matched a URL: %v", auth)
	default:
	}
	// The request event arrives now; the buffered Authorization is merged in and
	// the capture completes.
	w.handle(requestEvent("1", BaseURL+"/backend-api/conversations?offset=0", "GET",
		network.Headers{"chatgpt-account-id": "acct-1"}))
	select {
	case auth := <-w.ch:
		want := map[string]string{"Authorization": "Bearer wire", "chatgpt-account-id": "acct-1"}
		if !reflect.DeepEqual(auth, want) {
			t.Errorf("auth = %v, want %v", auth, want)
		}
	default:
		t.Fatal("buffered ExtraInfo was not merged when the request event arrived")
	}
}

func TestAuthWatcherSignalsOnlyOnce(t *testing.T) {
	w := newAuthWatcher("/backend-api/conversations?")
	for i := 0; i < 5; i++ {
		w.handle(requestEvent(network.RequestID(string(rune('a'+i))),
			BaseURL+"/backend-api/conversations?offset=0", "GET",
			network.Headers{"authorization": "Bearer tok"}))
	}
	if len(w.ch) != 1 {
		t.Errorf("channel holds %d results, want exactly 1", len(w.ch))
	}
}
