package browser

import (
	"context"
	"testing"
)

// flagValue returns the value of the named flag and whether it is present.
func flagValue(flags []flagSpec, name string) (any, bool) {
	for _, f := range flags {
		if f.Name == name {
			return f.Value, true
		}
	}
	return nil, false
}

// The launch flags are safety-critical: a regression here can get the user's
// account flagged. These assertions are the contract.
func TestAllocatorFlagsRequired(t *testing.T) {
	flags := allocatorFlags("/tmp/profile")

	tests := []struct {
		name string
		want any
	}{
		{"disable-blink-features", "AutomationControlled"},
		{"user-data-dir", "/tmp/profile"},
		{"no-first-run", true},
		{"no-default-browser-check", true},
	}
	for _, tt := range tests {
		got, ok := flagValue(flags, tt.name)
		if !ok {
			t.Errorf("missing required flag --%s", tt.name)
			continue
		}
		if got != tt.want {
			t.Errorf("flag --%s = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// Anything that tells the page it is being automated is forbidden. The
// values chromedp's DefaultExecAllocatorOptions would set are listed here
// explicitly so reintroducing them fails loudly.
func TestAllocatorFlagsForbidden(t *testing.T) {
	forbidden := []string{
		"headless",           // ChatGPT blocks headless browsers outright
		"enable-automation",  // sets the "controlled by test software" bit
		"disable-extensions", // fingerprintable, and not what a human runs
		"window-size",        // no viewport emulation (no_viewport=True)
		"user-agent",         // never spoof the UA of the real browser
		"hide-scrollbars",
		"mute-audio",
		"remote-debugging-pipe",
	}
	for _, name := range forbidden {
		if v, ok := flagValue(allocatorFlags("/tmp/profile"), name); ok {
			t.Errorf("forbidden flag --%s is set (value %v)", name, v)
		}
	}
}

// The flags must not be derived from chromedp's defaults: that list is
// exactly where --headless and --enable-automation come from.
func TestAllocatorFlagsAreNotChromedpDefaults(t *testing.T) {
	if got := len(allocatorFlags("/tmp/profile")); got > 6 {
		t.Errorf("allocatorFlags returned %d flags; the hand-picked set is tiny by "+
			"design — did chromedp.DefaultExecAllocatorOptions creep back in?", got)
	}
}

// allocatorOptions must pass through every flag plus the executable path,
// with nothing extra bolted on.
func TestAllocatorOptionsCoversEveryFlag(t *testing.T) {
	opts := allocatorOptions(Options{ProfileDir: "/tmp/profile", ExecPath: "/bin/browser"})
	if want := len(allocatorFlags("/tmp/profile")) + 1; len(opts) != want {
		t.Errorf("allocatorOptions returned %d options, want %d (flags + ExecPath)", len(opts), want)
	}
}

func TestLaunchRejectsIncompleteOptions(t *testing.T) {
	if _, err := Launch(context.Background(), Options{ProfileDir: "/tmp/p"}); err == nil {
		t.Error("Launch without ExecPath should fail rather than let chromedp guess a browser")
	}
	if _, err := Launch(context.Background(), Options{ExecPath: "/bin/browser"}); err == nil {
		t.Error("Launch without ProfileDir should fail rather than use a throwaway profile")
	}
}

func TestWebdriverOK(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"false", true},
		{"null", true},
		{"", true}, // undefined: the property does not exist
		{"true", false},
		{`"true"`, false},
		{"1", false},
	}
	for _, tt := range tests {
		if got := webdriverOK([]byte(tt.raw)); got != tt.want {
			t.Errorf("webdriverOK(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

func TestWebdriverText(t *testing.T) {
	tests := []struct{ raw, want string }{
		{"", "undefined"},
		{"null", "null"},
		{"true", "true"},
		{"false", "false"},
	}
	for _, tt := range tests {
		if got := webdriverText([]byte(tt.raw)); got != tt.want {
			t.Errorf("webdriverText(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}
