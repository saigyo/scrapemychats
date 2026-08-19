package browser

import (
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
)

func TestParseSessionState(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantNil bool
		wantErr bool
	}{
		{name: "logged in", raw: `{"user":{"email":"a@b.c"},"expires":"2026-01-01"}`},
		{name: "logged out returns null", raw: `null`, wantNil: true},
		{name: "undefined", raw: ``, wantNil: true},
		{name: "empty object", raw: `{}`},
		{name: "garbage", raw: `{oops`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, err := parseSessionState([]byte(tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSessionState: %v", err)
			}
			if tt.wantNil && state != nil {
				t.Errorf("expected nil state, got %v", state)
			}
		})
	}
}

func TestStateEmail(t *testing.T) {
	tests := []struct {
		name      string
		state     map[string]any
		wantEmail string
		wantIn    bool
	}{
		{
			name:      "email present",
			state:     map[string]any{"user": map[string]any{"email": "a@b.c"}},
			wantEmail: "a@b.c", wantIn: true,
		},
		{
			// export_chats.py:86 prints "?" when the user object has no email.
			name:      "user without email",
			state:     map[string]any{"user": map[string]any{"id": "u-1"}},
			wantEmail: "?", wantIn: true,
		},
		{
			// Python's `state.get("user")` is falsy for an empty dict, so
			// ensure_logged_in keeps polling instead of starting an export
			// with no credentials.
			name:  "empty user object is not a login",
			state: map[string]any{"user": map[string]any{}},
		},
		{name: "no user key", state: map[string]any{"expires": "soon"}},
		{name: "user is null", state: map[string]any{"user": nil}},
		{name: "user is not an object", state: map[string]any{"user": "nope"}},
		{name: "nil state", state: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			email, in := stateEmail(tt.state)
			if in != tt.wantIn || email != tt.wantEmail {
				t.Errorf("stateEmail = (%q, %v), want (%q, %v)", email, in, tt.wantEmail, tt.wantIn)
			}
		})
	}
}

// The banner is user-facing copy carried over verbatim from
// export_chats.py:88-94.
func TestLoginBanner(t *testing.T) {
	if len(loginBanner) != 7 {
		t.Fatalf("banner has %d lines, want 7", len(loginBanner))
	}
	if loginBanner[0] != "" {
		t.Error("banner should start with a blank line")
	}
	for _, i := range []int{1, 6} {
		if len(loginBanner[i]) != 64 || strings.Trim(loginBanner[i], "=") != "" {
			t.Errorf("line %d is not a 64-character rule: %q", i, loginBanner[i])
		}
	}
	joined := strings.Join(loginBanner, "\n")
	for _, want := range []string{
		"Not logged in.",
		"multiple workspaces",
		"checked every 5 seconds",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("banner is missing %q", want)
		}
	}
}

func TestSessionCloseRunsCancelsInReverse(t *testing.T) {
	var order []int
	s := &Session{}
	for i := range 3 {
		s.cancels = append(s.cancels, func() { order = append(order, i) })
	}
	s.Close()
	want := []int{2, 1, 0}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("cancel order = %v, want %v (the page must be closed before the allocator)", order, want)
		}
	}
	// Close must be idempotent: the export loop defers it and may also call
	// it explicitly.
	s.Close()
	if len(order) != 3 {
		t.Errorf("second Close re-ran cancels: %v", order)
	}
}

func TestLoadWatcherMatchesLoader(t *testing.T) {
	w := newLoadWatcher()
	w.handle(&page.EventLifecycleEvent{LoaderID: "L1", Name: "DOMContentLoaded"})
	// Events for other loaders, and other lifecycle names, are ignored.
	w.handle(&page.EventLifecycleEvent{LoaderID: "L2", Name: "load"})
	if w.watch("L2") {
		t.Error("watch(L2) reported an early DOMContentLoaded it never saw")
	}
	w2 := newLoadWatcher()
	w2.handle(&page.EventLifecycleEvent{LoaderID: "L1", Name: "DOMContentLoaded"})
	if !w2.watch("L1") {
		t.Error("a DOMContentLoaded that arrived before the navigate reply was lost")
	}
	select {
	case <-w2.finished():
	default:
		t.Error("finished() should already be closed")
	}
}

func TestLoadWatcherSignalsAfterWatch(t *testing.T) {
	w := newLoadWatcher()
	if w.watch("L1") {
		t.Fatal("nothing seen yet")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.handle(&page.EventLifecycleEvent{LoaderID: cdp.LoaderID("L1"), Name: "DOMContentLoaded"})
	}()
	<-w.finished()
	wg.Wait()
}

// Duplicate events must not panic on a double close.
func TestLoadWatcherRepeatedEvents(t *testing.T) {
	w := newLoadWatcher()
	w.watch("L1")
	for range 3 {
		w.handle(&page.EventLifecycleEvent{LoaderID: "L1", Name: "DOMContentLoaded"})
	}
}
