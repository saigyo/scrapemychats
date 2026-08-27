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
			ev:   respEvent(429, "https://chatgpt.com/backend-api/conversation/abc"),
			want: 1,
		},
		{
			name: "200 on a backend-api URL does not count",
			ev:   respEvent(200, "https://chatgpt.com/backend-api/conversation/abc"),
			want: 0,
		},
		{
			name: "429 on a non-backend URL does not count",
			ev:   respEvent(429, "https://chatgpt.com/static/app.js"),
			want: 0,
		},
		{
			name: "429 on a chat page URL does not count",
			ev:   respEvent(429, "https://chatgpt.com/c/some-conversation-id"),
			want: 0,
		},
		{
			name: "403 on a backend-api URL does not count (permissions, not rate-limit)",
			ev:   respEvent(403, "https://chatgpt.com/backend-api/conversation/abc"),
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
	s.countThrottled(respEvent(429, "https://chatgpt.com/backend-api/conversation/init"))
	s.countThrottled(respEvent(429, "https://chatgpt.com/backend-api/textdocs"))
	s.countThrottled(respEvent(200, "https://chatgpt.com/backend-api/conversation/abc"))

	if got := s.TakeThrottleHits(); got != 2 {
		t.Fatalf("first TakeThrottleHits() = %d, want 2", got)
	}
	if got := s.TakeThrottleHits(); got != 0 {
		t.Errorf("second TakeThrottleHits() = %d, want 0 (must reset)", got)
	}
}
