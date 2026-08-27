package export

import (
	"github.com/saigyo/scrapemychats/internal/browser"
	"github.com/saigyo/scrapemychats/internal/files"
)

// sessionBrowser is the production Browser: a thin adapter over a live,
// headed *browser.Session. Every method forwards to the corresponding
// browser/files package function, so the export loop's only real dependency is
// this handful of calls (which the test fake stands in for).
type sessionBrowser struct {
	s *browser.Session
}

// NewSessionBrowser adapts a launched browser session to the Browser
// interface the export loop drives.
func NewSessionBrowser(s *browser.Session) Browser {
	return &sessionBrowser{s: s}
}

func (b *sessionBrowser) EnsureLoggedIn() error {
	return browser.EnsureLoggedIn(b.s)
}

func (b *sessionBrowser) CaptureAuth() (map[string]string, error) {
	return browser.CaptureAuth(b.s)
}

func (b *sessionBrowser) CaptureConversation(url, cid string) (map[string]any, map[string]string, int, error) {
	return browser.CaptureConversation(b.s, url, cid)
}

func (b *sessionBrowser) Client() files.Client {
	return files.NewSessionClient(b.s)
}

func (b *sessionBrowser) TakeThrottleHits() int { return b.s.TakeThrottleHits() }

// Fetcher exposes the session as the in-page GET primitive discovery needs;
// *browser.Session already satisfies browser.Fetcher via its Fetch method.
func (b *sessionBrowser) Fetcher() browser.Fetcher {
	return b.s
}
