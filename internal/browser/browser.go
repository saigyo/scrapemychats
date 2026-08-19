// Package browser drives the user's own, already-installed Chrome (or Edge)
// through the Chrome DevTools Protocol, replacing the Playwright layer of
// export_chats.py.
//
// Everything here exists to look exactly like a human browsing ChatGPT:
//
//   - the browser is ALWAYS headed. ChatGPT blocks headless browsers, and a
//     headless run risks the user's Cloudflare standing and, ultimately,
//     their account.
//   - the browser is the user's real, installed Chrome/Edge, driven against a
//     persistent profile directory, so the login survives between runs.
//   - the command line is built from scratch (see allocatorFlags) rather than
//     from chromedp.DefaultExecAllocatorOptions, because those defaults pass
//     --enable-automation and --headless, both of which are trivially
//     detectable and forbidden here.
//   - no viewport/device emulation is ever applied, mirroring Playwright's
//     no_viewport=True in export_chats.py:732.
//
// The credentials never leave the browser: conversation JSON is captured from
// the responses the ChatGPT frontend fetches for itself, and every extra API
// call is made with fetch() *inside* the page, so cookies and TLS fingerprint
// match the real session (see fetch.go).
package browser

import "time"

// BaseURL is the ChatGPT origin, matching export_chats.py:41.
const BaseURL = "https://chatgpt.com"

// NavTimeout is how long a navigation + capture may take before it is
// treated as a timeout, matching NAV_TIMEOUT_MS = 45_000 in
// export_chats.py:50.
const NavTimeout = 45 * time.Second

// sleep is time.Sleep, indirected through a package variable so tests can
// run the throttling/login loops without actually waiting.
var sleep = time.Sleep
