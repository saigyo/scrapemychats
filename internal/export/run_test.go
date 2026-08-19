package export

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saigyo/scrapemychats/internal/browser"
	"github.com/saigyo/scrapemychats/internal/files"
)

// ---------------------------------------------------------------------------
// test seams
// ---------------------------------------------------------------------------

// recordSleeps installs the package sleep and uniform seams for one test: no
// real waiting, and the random component pinned to its lower bound so the
// pacing math is exactly predictable. The recorded durations let a test assert
// how the per-chat delay grows.
func recordSleeps(t *testing.T) *[]time.Duration {
	t.Helper()
	var got []time.Duration
	origSleep, origUniform := sleep, uniform
	sleep = func(ctx context.Context, d time.Duration) error {
		got = append(got, d)
		return ctx.Err() // honor cancellation like the real cancellable sleep
	}
	uniform = func(min, max float64) float64 { return min } // deterministic lower bound
	t.Cleanup(func() { sleep, uniform = origSleep, origUniform })
	return &got
}

// ---------------------------------------------------------------------------
// fake Browser
// ---------------------------------------------------------------------------

// scriptedCapture is one programmed response for a conversation id. When
// attempts is set, the leading N calls for that id return throttle/error and
// the final call returns data — so a chat can exercise the retry/backoff path.
type scriptedCapture struct {
	// queue of results returned in order for this cid; the last entry repeats.
	results []captureResult
	calls   int
}

type captureResult struct {
	data   map[string]any
	status int
	err    error
}

type fakeBrowser struct {
	loggedIn bool
	auth     map[string]string
	captures map[string]*scriptedCapture
	client   files.Client
	fetcher  browser.Fetcher
}

func (b *fakeBrowser) EnsureLoggedIn() error { b.loggedIn = true; return nil }

func (b *fakeBrowser) CaptureAuth() (map[string]string, error) { return b.auth, nil }

func (b *fakeBrowser) CaptureConversation(url, cid string) (map[string]any, map[string]string, int, error) {
	sc := b.captures[cid]
	if sc == nil {
		return nil, nil, 404, nil
	}
	i := sc.calls
	if i >= len(sc.results) {
		i = len(sc.results) - 1
	}
	sc.calls++
	r := sc.results[i]
	if r.err != nil {
		return nil, nil, 0, r.err
	}
	if r.status != 200 {
		return nil, nil, r.status, nil
	}
	return r.data, b.auth, 200, nil
}

func (b *fakeBrowser) Client() files.Client     { return b.client }
func (b *fakeBrowser) Fetcher() browser.Fetcher { return b.fetcher }

// ---------------------------------------------------------------------------
// fake files.Client
// ---------------------------------------------------------------------------

// fakeFilesClient scripts the network the file downloaders use. Fetch returns
// metadata with an empty download_url (so DownloadFiles records a failure and,
// per the Python port, skips its trailing sleep — keeping the test instant),
// and PostJSON returns an empty Library so the sweep does nothing.
type fakeFilesClient struct {
	libraryItems []map[string]any
}

func (c *fakeFilesClient) Fetch(u string, h map[string]string) (int, string, error) {
	// 200 with a parseable body but no download_url: "expired file" path.
	return 200, `{"download_url": ""}`, nil
}

func (c *fakeFilesClient) PostJSON(u string, h map[string]string, body any) (int, string, error) {
	items := c.libraryItems
	if items == nil {
		items = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	return 200, string(b), nil
}

func (c *fakeFilesClient) GetBinary(u string) (int, []byte, string, error) {
	return 200, []byte("x"), "application/octet-stream", nil
}

func (c *fakeFilesClient) GetBinaryInPage(u string, h map[string]string) (int, []byte, error) {
	return 200, []byte("x"), nil
}

// cancelDownloadClient simulates Ctrl+C arriving while a chat's files are
// downloading: its metadata Fetch cancels the context (then returns an empty
// download_url so the download records a failure and skips its trailing sleep,
// keeping the test instant). It embeds fakeFilesClient for the other methods.
type cancelDownloadClient struct {
	fakeFilesClient
	cancel context.CancelFunc
}

func (c *cancelDownloadClient) Fetch(u string, h map[string]string) (int, string, error) {
	c.cancel()
	return 200, `{"download_url": ""}`, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeCSV(t *testing.T, path string, rows [][2]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"url", "title"})
	for _, r := range rows {
		_ = w.Write([]string{r[0], r[1]})
	}
	w.Flush()
}

// smallConv builds a minimal conversation document with one user message and
// (optionally) one attachment file ref, in the shape convmd/files expect.
func smallConv(cid, text, fileID, fileName string) map[string]any {
	msgID := "msg-" + cid
	meta := map[string]any{}
	if fileID != "" {
		meta["attachments"] = []any{map[string]any{"id": fileID, "name": fileName}}
	}
	return map[string]any{
		"title":           "Conv " + cid,
		"conversation_id": cid,
		"current_node":    msgID,
		"mapping": map[string]any{
			msgID: map[string]any{
				"id":     msgID,
				"parent": nil,
				"message": map[string]any{
					"id":       msgID,
					"author":   map[string]any{"role": "user"},
					"metadata": meta,
					"content":  map[string]any{"content_type": "text", "parts": []any{text}},
				},
			},
		},
	}
}

func readManifest(t *testing.T, outDir string) [][]string {
	t.Helper()
	f, err := os.Open(filepath.Join(outDir, "manifest.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// ---------------------------------------------------------------------------
// the main end-to-end loop test
// ---------------------------------------------------------------------------

func TestExportLoop(t *testing.T) {
	got := recordSleeps(t)
	outDir := t.TempDir()
	csvPath := filepath.Join(outDir, "chats.csv")

	base := browser.BaseURL
	// Four chats:
	//  001 already exported (resume skip)
	//  002 plain 200 with a file ref (download attempted, records a failure)
	//  003 throttled twice then 200 (exercises backoff + permanent slowdown)
	//  004 fails all attempts (failed manifest row)
	cids := []string{
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		"cccccccc-cccc-cccc-cccc-cccccccccccc",
		"dddddddd-dddd-dddd-dddd-dddddddddddd",
	}
	writeCSV(t, csvPath, [][2]string{
		{base + "/c/" + cids[0], "Alpha"},
		{base + "/c/" + cids[1], "Beta"},
		{base + "/c/" + cids[2], "Gamma"},
		{base + "/c/" + cids[3], "Delta"},
	})

	// Pre-create chat 001's folder + marker so it is skipped on resume.
	skipFolder := filepath.Join(outDir, "001_Alpha_"+cids[0][:8])
	if err := os.MkdirAll(skipFolder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skipFolder, "conversation.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	fb := &fakeBrowser{
		auth:   map[string]string{"Authorization": "Bearer x"},
		client: &fakeFilesClient{},
		captures: map[string]*scriptedCapture{
			cids[1]: {results: []captureResult{{data: smallConv(cids[1], "hello beta", "file-BETA123", "beta.txt"), status: 200}}},
			cids[2]: {results: []captureResult{
				{status: 429},
				{status: 429},
				{data: smallConv(cids[2], "hello gamma", "", ""), status: 200},
			}},
			cids[3]: {results: []captureResult{
				{status: 500}, {status: 500}, {status: 500},
			}},
		},
	}

	sum, err := Export(context.Background(), fb, Config{CSVPath: csvPath, OutDir: outDir})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// --- summary counts ---
	if sum.Exported != 2 || sum.Skipped != 1 || sum.Failed != 1 {
		t.Errorf("summary = %+v; want exported=2 skipped=1 failed=1", sum)
	}
	if !fb.loggedIn {
		t.Error("EnsureLoggedIn was never called")
	}

	// --- folder names / numbering ---
	betaFolder := filepath.Join(outDir, "002_Beta_"+cids[1][:8])
	gammaFolder := filepath.Join(outDir, "003_Gamma_"+cids[2][:8])
	for _, f := range []string{betaFolder, gammaFolder} {
		if _, err := os.Stat(filepath.Join(f, "conversation.json")); err != nil {
			t.Errorf("expected marker in %s: %v", f, err)
		}
		if _, err := os.Stat(filepath.Join(f, "conversation.md")); err != nil {
			t.Errorf("expected conversation.md in %s: %v", f, err)
		}
	}
	// Delta failed → no folder created.
	if _, err := os.Stat(filepath.Join(outDir, "004_Delta_"+cids[3][:8])); !os.IsNotExist(err) {
		t.Errorf("failed chat should not have a folder, stat err=%v", err)
	}

	// --- conversation.md content came from convmd ---
	md, err := os.ReadFile(filepath.Join(betaFolder, "conversation.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "hello beta") || !strings.Contains(string(md), "## User") {
		t.Errorf("conversation.md missing rendered content:\n%s", md)
	}
	if !strings.Contains(string(md), "- attached: `beta.txt`") {
		t.Errorf("conversation.md missing attachment line:\n%s", md)
	}

	// --- conversation.json is valid JSON (resume marker) ---
	cj, err := os.ReadFile(filepath.Join(betaFolder, "conversation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal(cj, &round); err != nil {
		t.Errorf("conversation.json not valid JSON: %v", err)
	}
	if round["conversation_id"] != cids[1] {
		t.Errorf("conversation.json conversation_id = %v", round["conversation_id"])
	}

	// --- manifest rows exact ---
	rows := readManifest(t, outDir)
	want := [][]string{
		{"url", "title", "folder", "status", "messages", "files_ok", "files_failed"},
		{base + "/c/" + cids[1], "Beta", "002_Beta_" + cids[1][:8], "ok", "1", "0", "1"},
		{base + "/c/" + cids[2], "Gamma", "003_Gamma_" + cids[2][:8], "ok", "1", "0", "0"},
		{base + "/c/" + cids[3], "Delta", "", "failed", "0", "0", "0"},
	}
	if len(rows) != len(want) {
		t.Fatalf("manifest has %d rows, want %d:\n%v", len(rows), len(want), rows)
	}
	for i := range want {
		if strings.Join(rows[i], "|") != strings.Join(want[i], "|") {
			t.Errorf("manifest row %d = %v\n            want %v", i, rows[i], want[i])
		}
	}

	// --- errors.log entries ---
	el, err := os.ReadFile(filepath.Join(outDir, "errors.log"))
	if err != nil {
		t.Fatal(err)
	}
	els := string(el)
	// Beta's expired file:
	if !strings.Contains(els, "[Beta] "+base+"/c/"+cids[1]) ||
		!strings.Contains(els, "no download_url") {
		t.Errorf("errors.log missing Beta file failure:\n%s", els)
	}
	// Delta's three HTTP 500 attempts:
	for _, n := range []int{1, 2, 3} {
		want := fmt.Sprintf("conversation HTTP 500 (attempt %d)", n)
		if !strings.Contains(els, want) {
			t.Errorf("errors.log missing %q:\n%s", want, els)
		}
	}
	if !strings.Contains(els, "[Delta] "+base+"/c/"+cids[3]) {
		t.Errorf("errors.log missing Delta chat header:\n%s", els)
	}

	// --- pacing math ---
	// Two chats exported, neither is the 20th, so no long break fired. The
	// long break sleeps exactly seconds(LongBreakMin) with uniform pinned low
	// (distinct from the 300s/600s rate-limit backoffs).
	for _, d := range *got {
		if d == seconds(LongBreakMin) {
			t.Errorf("unexpected long-break sleep %v (no 20th chat)", d)
		}
	}
	// Beta exported with rlHits=0 → per-chat delay == DelayMin (uniform pinned
	// to min, extraDelay 0). Gamma exported after 2 throttles → extraDelay =
	// ExtraDelayPer429*2 = 10, per-chat delay == DelayMin + 10.
	betaDelay := seconds(DelayMin)
	gammaDelay := seconds(DelayMin + ExtraDelayPer429*2)
	if !containsDuration(*got, betaDelay) {
		t.Errorf("expected a per-chat delay of %v (Beta); got %v", betaDelay, *got)
	}
	if !containsDuration(*got, gammaDelay) {
		t.Errorf("expected a grown per-chat delay of %v (Gamma, after 2x429); got %v", gammaDelay, *got)
	}
	// The two 429 backoffs (300s, 600s) must have been slept.
	if !containsDuration(*got, 300*time.Second) || !containsDuration(*got, 600*time.Second) {
		t.Errorf("expected the 300s and 600s rate-limit backoffs; got %v", *got)
	}
}

func containsDuration(ds []time.Duration, want time.Duration) bool {
	for _, d := range ds {
		if d == want {
			return true
		}
	}
	return false
}

// TestExportLongBreak drives 20 successful chats to verify the long breather
// fires exactly on the 20th exported chat.
func TestExportLongBreak(t *testing.T) {
	got := recordSleeps(t)
	outDir := t.TempDir()
	csvPath := filepath.Join(outDir, "chats.csv")

	var rows [][2]string
	captures := map[string]*scriptedCapture{}
	for i := 0; i < LongBreakEvery; i++ {
		cid := fmt.Sprintf("%08d-0000-0000-0000-000000000000", i)
		rows = append(rows, [2]string{browser.BaseURL + "/c/" + cid, fmt.Sprintf("Chat%02d", i)})
		captures[cid] = &scriptedCapture{results: []captureResult{{data: smallConv(cid, "hi", "", ""), status: 200}}}
	}
	writeCSV(t, csvPath, rows)

	fb := &fakeBrowser{auth: map[string]string{"Authorization": "x"}, client: &fakeFilesClient{}, captures: captures}
	sum, err := Export(context.Background(), fb, Config{CSVPath: csvPath, OutDir: outDir})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Exported != LongBreakEvery {
		t.Fatalf("exported=%d, want %d", sum.Exported, LongBreakEvery)
	}
	// Exactly one long-break-sized sleep (== LongBreakMin, uniform pinned low).
	long := 0
	for _, d := range *got {
		if d == seconds(LongBreakMin) {
			long++
		}
	}
	if long != 1 {
		t.Errorf("long-break sleeps = %d, want 1; sleeps=%v", long, *got)
	}
}

// TestExportRediscover verifies that a missing CSV triggers discovery via the
// injected Fetcher before the (empty) export proceeds.
func TestExportRediscover(t *testing.T) {
	recordSleeps(t)
	outDir := t.TempDir()
	csvPath := filepath.Join(outDir, "chats.csv") // does not exist

	ff := &fetchFake{}
	fb := &fakeBrowser{auth: map[string]string{"Authorization": "x"}, client: &fakeFilesClient{}, fetcher: ff, captures: map[string]*scriptedCapture{}}

	if _, err := Export(context.Background(), fb, Config{CSVPath: csvPath, OutDir: outDir}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !fileExists(csvPath) {
		t.Error("discovery should have created chats.csv")
	}
	if ff.calls == 0 {
		t.Error("discovery never used the injected Fetcher")
	}
}

// fetchFake is a browser.Fetcher returning an empty conversation list, so
// DiscoverChats writes a header-only chats.csv without a real browser.
type fetchFake struct{ calls int }

func (f *fetchFake) Fetch(u string, h map[string]string) (int, string, error) {
	f.calls++
	if strings.Contains(u, "/backend-api/conversations?") {
		return 200, `{"items": [], "total": 0}`, nil
	}
	// gizmos sidebar / anything else: empty.
	return 200, `{"items": [], "cursor": null}`, nil
}

// TestFixFilesMode drives the fix-files happy path over one exported folder
// whose files are all already present, so no downloads run.
func TestFixFilesMode(t *testing.T) {
	recordSleeps(t)
	outDir := t.TempDir()

	cid := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	folder := filepath.Join(outDir, "001_Fix_"+cid[:8])
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	// A conversation with no file refs → FixFiles finds nothing to do.
	conv := smallConv(cid, "no files here", "", "")
	cb, _ := json.Marshal(conv)
	if err := os.WriteFile(filepath.Join(folder, "conversation.json"), cb, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "conversation.md"), []byte("# Fix\n\n- URL: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fb := &fakeBrowser{auth: map[string]string{"Authorization": "x"}, client: &fakeFilesClient{}}
	if err := FixFilesMode(context.Background(), fb, Config{OutDir: outDir}); err != nil {
		t.Fatalf("FixFilesMode: %v", err)
	}
	if !fb.loggedIn {
		t.Error("FixFilesMode did not ensure login")
	}
}

// cancelBrowser exports the first chat successfully, then on the SECOND
// CaptureConversation call cancels the context and returns context.Canceled —
// standing in for the session ctx.Err() a live browser returns on SIGINT. It
// records how many captures were attempted so a test can prove the loop stopped
// early instead of grinding through the rest of the list.
type cancelBrowser struct {
	auth     map[string]string
	client   files.Client
	cancel   context.CancelFunc
	cancelAt int
	calls    int
	data     map[string]any
}

func (b *cancelBrowser) EnsureLoggedIn() error                   { return nil }
func (b *cancelBrowser) CaptureAuth() (map[string]string, error) { return b.auth, nil }
func (b *cancelBrowser) Client() files.Client                    { return b.client }
func (b *cancelBrowser) Fetcher() browser.Fetcher                { return nil }

func (b *cancelBrowser) CaptureConversation(url, cid string) (map[string]any, map[string]string, int, error) {
	b.calls++
	if b.calls >= b.cancelAt {
		b.cancel()
		return nil, nil, 0, context.Canceled
	}
	return b.data, b.auth, 200, nil
}

// TestExportInterrupted proves the fix for the blocking defect: when a capture
// is cancelled mid-run, Export returns a non-nil cancellation error, writes NO
// spurious "failed" rows for the interrupted or unreached chats, and stops
// promptly rather than attempting every remaining chat.
func TestExportInterrupted(t *testing.T) {
	recordSleeps(t)
	outDir := t.TempDir()
	csvPath := filepath.Join(outDir, "chats.csv")

	base := browser.BaseURL
	var rows [][2]string
	for i := 0; i < 5; i++ {
		cid := fmt.Sprintf("%08d-0000-0000-0000-000000000000", i)
		rows = append(rows, [2]string{base + "/c/" + cid, fmt.Sprintf("Chat%02d", i)})
	}
	writeCSV(t, csvPath, rows)

	ctx, cancel := context.WithCancel(context.Background())
	cb := &cancelBrowser{
		auth:     map[string]string{"Authorization": "x"},
		client:   &fakeFilesClient{},
		cancel:   cancel,
		cancelAt: 2, // succeed on chat 1, cancel during chat 2's capture
		data:     smallConv("00000000-0000-0000-0000-000000000000", "hi", "", ""),
	}

	sum, err := Export(ctx, cb, Config{CSVPath: csvPath, OutDir: outDir})

	// 1. Non-nil cancellation error propagated.
	if err == nil {
		t.Fatal("Export returned nil error on cancellation; want a cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Export error = %v; want context.Canceled", err)
	}

	// 2. Stopped promptly: only chats 1 and 2 were attempted, not all 5.
	if cb.calls != 2 {
		t.Errorf("CaptureConversation calls = %d; want 2 (stopped after cancellation)", cb.calls)
	}
	if sum.Exported != 1 {
		t.Errorf("summary.Exported = %d; want 1", sum.Exported)
	}

	// 3. No spurious "failed" rows: the manifest has only the header and the one
	// successful chat — nothing for the interrupted chat 2 or the unreached 3-5.
	manifestRows := readManifest(t, outDir)
	for _, r := range manifestRows {
		if len(r) >= 4 && r[3] == "failed" {
			t.Errorf("found a spurious failed manifest row after interruption: %v", r)
		}
	}
	if len(manifestRows) != 2 { // header + Chat00
		t.Errorf("manifest has %d rows, want 2 (header + one ok); rows=%v", len(manifestRows), manifestRows)
	}
}

// TestExportCancelledDuringDownloadsSkipsMarker verifies that a Ctrl+C landing
// while a chat's files download does NOT leave a conversation.json completion
// marker — otherwise a normal resume would skip the chat with files missing.
func TestExportCancelledDuringDownloadsSkipsMarker(t *testing.T) {
	recordSleeps(t) // pacing sleeps become instant and cancellation-aware
	outDir := t.TempDir()
	csvPath := filepath.Join(outDir, "chats.csv")
	cid := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	writeCSV(t, csvPath, [][2]string{{browser.BaseURL + "/c/" + cid, "Echo"}})

	ctx, cancel := context.WithCancel(context.Background())
	fb := &fakeBrowser{
		auth:   map[string]string{"Authorization": "Bearer x"},
		client: &cancelDownloadClient{cancel: cancel},
		captures: map[string]*scriptedCapture{
			cid: {results: []captureResult{{data: smallConv(cid, "hi echo", "file-ECHO", "echo.txt"), status: 200}}},
		},
	}

	if _, err := Export(ctx, fb, Config{CSVPath: csvPath, OutDir: outDir}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Export err = %v, want context.Canceled", err)
	}

	folder := filepath.Join(outDir, "001_Echo_"+cid[:8])
	// conversation.md is written before downloads, so it exists...
	if _, err := os.Stat(filepath.Join(folder, "conversation.md")); err != nil {
		t.Errorf("conversation.md should have been written before the download: %v", err)
	}
	// ...but the completion marker must NOT be written on cancellation.
	if _, err := os.Stat(filepath.Join(folder, "conversation.json")); !os.IsNotExist(err) {
		t.Errorf("conversation.json marker must NOT exist after cancellation; stat err=%v", err)
	}
}
