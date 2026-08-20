package main

import (
	"flag"
	"path/filepath"
	"strings"
	"testing"

	"github.com/saigyo/scrapemychats"
	"github.com/saigyo/scrapemychats/internal/browser"
)

func TestResolveBaseDirWritable(t *testing.T) {
	exe := filepath.Join("/opt", "app", "scrapemychats")
	base, note := resolveBaseDir(exe, func(string) bool { return true }, "/home/u")
	// EvalSymlinks fails on this non-existent path, so dir stays filepath.Dir(exe).
	want := filepath.Dir(exe)
	if base != want {
		t.Errorf("base = %q, want %q", base, want)
	}
	if note != "" {
		t.Errorf("note = %q, want empty on the writable path", note)
	}
}

func TestResolveBaseDirFallback(t *testing.T) {
	exe := "/Applications/scrapemychats.app/scrapemychats"
	base, note := resolveBaseDir(exe, func(string) bool { return false }, "/home/u")
	want := filepath.Join("/home/u", "Documents", "scrapemychats")
	if base != want {
		t.Errorf("base = %q, want %q", base, want)
	}
	if note == "" {
		t.Error("expected a note explaining the fallback")
	}
}

func TestResolveBaseDirEvalSymlinks(t *testing.T) {
	// A real, existing dir so EvalSymlinks resolves; writable=true keeps it.
	dir := t.TempDir()
	exe := filepath.Join(dir, "scrapemychats")
	base, _ := resolveBaseDir(exe, func(string) bool { return true }, "/home/u")
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if base != resolved {
		t.Errorf("base = %q, want %q (symlink-resolved)", base, resolved)
	}
}

func TestResolvePath(t *testing.T) {
	base := "/data"
	cases := []struct {
		flagVal, name, want string
	}{
		// default → base-relative (filepath.Join uses the OS separator).
		{"", "chats.csv", filepath.Join(base, "chats.csv")},
		{"/abs/my.csv", "chats.csv", "/abs/my.csv"}, // absolute honored verbatim
		{"rel/my.csv", "chats.csv", "rel/my.csv"},   // relative honored as-is
	}
	for _, c := range cases {
		if got := resolvePath(c.flagVal, base, c.name); got != c.want {
			t.Errorf("resolvePath(%q, %q, %q) = %q, want %q", c.flagVal, base, c.name, got, c.want)
		}
	}
}

func TestShouldPause(t *testing.T) {
	cases := []struct {
		noPause, tty, want bool
	}{
		{false, true, true},   // interactive, pause allowed → pause
		{true, true, false},   // --no-pause overrides
		{false, false, false}, // not a terminal → never pause
		{true, false, false},
	}
	for _, c := range cases {
		if got := shouldPause(c.noPause, c.tty); got != c.want {
			t.Errorf("shouldPause(noPause=%v, tty=%v) = %v, want %v", c.noPause, c.tty, got, c.want)
		}
	}
}

func TestFlagDefaults(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	o := registerFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if o.csv != "" || o.out != "" || o.profile != "" || o.categories != "" {
		t.Errorf("path flags should default empty (resolved against base later): %+v", o)
	}
	if o.limit != 0 {
		t.Errorf("limit default = %d, want 0", o.limit)
	}
	if o.channel != browser.ChannelChrome {
		t.Errorf("browser-channel default = %q, want %q", o.channel, browser.ChannelChrome)
	}
	if o.viewerTitle != "The Chat Archive" {
		t.Errorf("viewer-title default = %q, want %q", o.viewerTitle, "The Chat Archive")
	}
	if o.rediscover || o.fixFiles || o.viewerOnly || o.noPause || o.showVersion || o.showLicense {
		t.Errorf("bool flags should default false: %+v", o)
	}
}

func TestVersionFlag(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	o := registerFlags(fs)
	if err := fs.Parse([]string{"--version"}); err != nil {
		t.Fatal(err)
	}
	if !o.showVersion {
		t.Error("--version should set showVersion")
	}
	// run(["--version"]) must exit 0 without touching the browser.
	if code := run([]string{"--version"}); code != 0 {
		t.Errorf("run(--version) = %d, want 0", code)
	}
	// The default build stamp is "dev"; a release overrides it via ldflags.
	if version == "" {
		t.Error("version var should never be empty")
	}
}

func TestLicenseFlag(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	o := registerFlags(fs)
	if err := fs.Parse([]string{"--license"}); err != nil {
		t.Fatal(err)
	}
	if !o.showLicense {
		t.Error("--license should set showLicense")
	}
	// run(["--license"]) must exit 0 without touching the browser.
	if code := run([]string{"--license"}); code != 0 {
		t.Errorf("run(--license) = %d, want 0", code)
	}
	// The embedded notices must be present and look like the generated file
	// (tools/gen-licenses guarantees the preamble; its freshness gate keeps
	// the content in sync with the dependency graph).
	if !strings.HasPrefix(scrapemychats.ThirdPartyLicenses, "Third-party licenses for scrapemychats") {
		t.Errorf("embedded notices should start with the generated preamble, got %.60q", scrapemychats.ThirdPartyLicenses)
	}
	if !strings.Contains(scrapemychats.ThirdPartyLicenses, "github.com/chromedp/chromedp") {
		t.Error("embedded notices should cover the chromedp dependency")
	}
}

func TestFlagParsing(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	o := registerFlags(fs)
	args := []string{"--limit", "3", "--rediscover", "--browser-channel", "msedge",
		"--out", "/tmp/x", "--viewer-title", "My Archive", "--no-pause"}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if o.limit != 3 || !o.rediscover || o.channel != browser.ChannelEdge ||
		o.out != "/tmp/x" || o.viewerTitle != "My Archive" || !o.noPause {
		t.Errorf("parsed options unexpected: %+v", o)
	}
}

func TestAcquireLock(t *testing.T) {
	base := t.TempDir()
	release, ok := acquireLock(base)
	if !ok {
		t.Fatal("first acquireLock should succeed")
	}
	if _, ok2 := acquireLock(base); ok2 {
		t.Error("second concurrent acquireLock should fail while lock is held")
	}
	release()
	// After release the lock is free again.
	release2, ok3 := acquireLock(base)
	if !ok3 {
		t.Error("acquireLock should succeed after release")
	}
	release2()
}
