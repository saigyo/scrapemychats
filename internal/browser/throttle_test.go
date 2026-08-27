package browser

import (
	"testing"

	"github.com/chromedp/cdproto/network"
)

// respEvent builds a minimal *network.EventResponseReceived for the given
// status/URL, standing in for what chromedp's event goroutine would deliver.
func respEvent(status int64, url string) *network.EventResponseReceived {
	return &network.EventResponseReceived{
		Response: &network.Response{Status: status, URL: url},
	}
}

func TestCountThrottled(t *testing.T) {
	tests := []struct {
		name string
		ev   any
		want int
	}{
		{
			name: "429 on a backend-api URL increments",
			ev:   respEvent(429, BaseURL+"/backend-api/conversation/abc"),
			want: 1,
		},
		{
			name: "200 on a backend-api URL does not count",
			ev:   respEvent(200, BaseURL+"/backend-api/conversation/abc"),
			want: 0,
		},
		{
			name: "429 on a non-backend URL does not count",
			ev:   respEvent(429, BaseURL+"/static/app.js"),
			want: 0,
		},
		{
			name: "429 on a chat page URL does not count",
			ev:   respEvent(429, BaseURL+"/c/some-conversation-id"),
			want: 0,
		},
		{
			name: "403 on a backend-api URL does not count (permissions, not rate-limit)",
			ev:   respEvent(403, BaseURL+"/backend-api/conversation/abc"),
			want: 0,
		},
		{
			name: "an unrelated event type is ignored",
			ev:   "not a network event",
			want: 0,
		},
		{
			name: "a nil Response does not panic",
			ev:   &network.EventResponseReceived{Response: nil},
			want: 0,
		},
		{
			name: "429 on another host with a /backend-api/ path does not count",
			ev:   respEvent(429, "http://127.0.0.1:8080/backend-api/x"),
			want: 0,
		},
		{
			name: "429 whose /backend-api/ appears only in the query string does not count",
			ev:   respEvent(429, BaseURL+"/c/abc?next=/backend-api/conversation/x"),
			want: 0,
		},
		{
			name: "429 whose /backend-api/ appears only in the fragment does not count",
			ev:   respEvent(429, BaseURL+"/c/abc#/backend-api/conversation/x"),
			want: 0,
		},
		{
			name: "429 on a real backend URL with a query string increments",
			ev:   respEvent(429, BaseURL+"/backend-api/conversations?offset=0&limit=28"),
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Session{}
			s.countThrottled(tt.ev)
			if got := s.TakeThrottleHits(); got != tt.want {
				t.Errorf("TakeThrottleHits() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestTakeThrottleHitsResets proves TakeThrottleHits returns the accumulated
// count and resets it to zero, so a second call in a row sees no repeats.
func TestTakeThrottleHitsResets(t *testing.T) {
	s := &Session{}
	s.countThrottled(respEvent(429, BaseURL+"/backend-api/conversation/init"))
	s.countThrottled(respEvent(429, BaseURL+"/backend-api/textdocs"))
	s.countThrottled(respEvent(200, BaseURL+"/backend-api/conversation/abc"))

	if got := s.TakeThrottleHits(); got != 2 {
		t.Fatalf("first TakeThrottleHits() = %d, want 2", got)
	}
	if got := s.TakeThrottleHits(); got != 0 {
		t.Errorf("second TakeThrottleHits() = %d, want 0 (must reset)", got)
	}
}
