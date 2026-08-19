package main

import (
	"flag"
	"path/filepath"
	"testing"

	"github.com/saigyo/scrapemychats/internal/browser"
)

func TestResolveBaseDirWritable(t *testing.T) {
	exe := "/opt/app/scrapemychats"
	base, note := resolveBaseDir(exe, func(string) bool { return true }, "/home/u")
	// EvalSymlinks fails on this non-existent path, so dir stays filepath.Dir(exe).
	if base != "/opt/app" {
		t.Errorf("base = %q, want /opt/app", base)
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
		{"", "chats.csv", "/data/chats.csv"},        // default → base-relative
		{"/abs/my.csv", "chats.csv", "/abs/my.csv"}, // absolute honored
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
	if o.rediscover || o.fixFiles || o.viewerOnly || o.noPause {
		t.Errorf("bool flags should default false: %+v", o)
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
