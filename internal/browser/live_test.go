package browser

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestLiveBrowser starts the user's real, headed browser and checks the
// safety-critical properties that cannot be unit tested: that Chrome accepts
// our flag list, that it does not advertise itself as automated, and that
// in-page evaluation and fetch work.
//
// It never runs in CI (no display, and it would open a window). To run it
// locally:
//
//	SCRAPEMYCHATS_LIVE_BROWSER=1 go test ./internal/browser/ -run TestLiveBrowser -v
//
// A browser window flashes open on about:blank and closes again; a throwaway
// profile directory under the test's temp dir is used, so the real
// browser_profile/ and its ChatGPT login are untouched.
func TestLiveBrowser(t *testing.T) {
	if os.Getenv("SCRAPEMYCHATS_LIVE_BROWSER") == "" {
		t.Skip("set SCRAPEMYCHATS_LIVE_BROWSER=1 to run this (opens a real browser window)")
	}

	execPath, err := FindBrowser(os.Getenv("SCRAPEMYCHATS_LIVE_CHANNEL"))
	if err != nil {
		t.Fatalf("FindBrowser: %v", err)
	}
	t.Logf("driving %s", execPath)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	s, err := Launch(ctx, Options{ProfileDir: throwawayProfile(t), ExecPath: execPath})
	if err != nil {
		// Launch itself fails when navigator.webdriver is true, which is
		// the regression this test exists to catch.
		t.Fatalf("Launch: %v", err)
	}
	defer s.Close()

	if err := navigate(s.Context(), "about:blank", NavTimeout); err != nil {
		t.Fatalf("navigate(about:blank): %v", err)
	}

	var webdriver any
	if err := chromedp.Run(s.Context(),
		chromedp.Evaluate("navigator.webdriver", &webdriver),
	); err != nil {
		t.Fatalf("evaluating navigator.webdriver: %v", err)
	}
	if webdriver != nil && webdriver != false {
		t.Fatalf("navigator.webdriver = %v, want false/undefined — the browser is "+
			"advertising automation and ChatGPT would flag the session", webdriver)
	}

	var two int
	if err := chromedp.Run(s.Context(), chromedp.Evaluate("1 + 1", &two)); err != nil {
		t.Fatalf("in-page evaluation failed: %v", err)
	}
	if two != 2 {
		t.Fatalf("1 + 1 = %d", two)
	}

	// Exercise the whole fetch path (expression building, awaited promise,
	// result parsing) without touching the network.
	status, body, err := FetchWithSession(s, "data:text/plain,hello", nil)
	if err != nil {
		t.Fatalf("FetchWithSession: %v", err)
	}
	if status != 200 || body != "hello" {
		t.Fatalf("in-page fetch returned (%d, %q), want (200, \"hello\")", status, body)
	}
}

// throwawayProfile returns a scratch user-data directory for the live test.
//
// t.TempDir is deliberately not used: its cleanup fails the test if the
// directory is not empty, and Chrome's helper processes (GPU, crashpad) keep
// writing into the profile for a moment after the browser process itself has
// exited. Cleanup here is best-effort and never fails the test — the
// directory lives under the system temp dir either way.
func throwawayProfile(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "scrapemychats-live-profile-")
	if err != nil {
		t.Fatalf("creating a throwaway profile directory: %v", err)
	}
	t.Cleanup(func() {
		for range 5 {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Logf("left %s behind (Chrome was still writing to it)", dir)
	})
	return dir
}
