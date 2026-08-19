package files

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/saigyo/scrapemychats/internal/browser"
)

// *browser.Session must be usable as a Client through the adapter.
var _ Client = (*sessionClient)(nil)

// noSleep replaces the package sleep with a no-op for one test, so the
// download loops never wait.
func noSleep(t *testing.T) {
	t.Helper()
	orig := sleep
	sleep = func(time.Duration) {}
	t.Cleanup(func() { sleep = orig })
}

// fakeClient is a scripted Client. Each method delegates to the matching
// func field (a nil field is a test bug — the method fails loudly). It also
// records the URLs and POST bodies it saw.
type fakeClient struct {
	fetch    func(u string, h map[string]string) (int, string, error)
	postJSON func(u string, h map[string]string, body any) (int, string, error)
	getBin   func(u string) (int, []byte, string, error)
	getBinIP func(u string, h map[string]string) (int, []byte, error)

	fetchURLs  []string
	postBodies []any
	getBinURLs []string
}

func (f *fakeClient) Fetch(u string, h map[string]string) (int, string, error) {
	f.fetchURLs = append(f.fetchURLs, u)
	if f.fetch == nil {
		return 0, "", fmt.Errorf("fakeClient.Fetch not scripted (url=%s)", u)
	}
	return f.fetch(u, h)
}

func (f *fakeClient) PostJSON(u string, h map[string]string, body any) (int, string, error) {
	f.postBodies = append(f.postBodies, body)
	if f.postJSON == nil {
		return 0, "", fmt.Errorf("fakeClient.PostJSON not scripted (url=%s)", u)
	}
	return f.postJSON(u, h, body)
}

func (f *fakeClient) GetBinary(u string) (int, []byte, string, error) {
	f.getBinURLs = append(f.getBinURLs, u)
	if f.getBin == nil {
		return 0, nil, "", fmt.Errorf("fakeClient.GetBinary not scripted (url=%s)", u)
	}
	return f.getBin(u)
}

func (f *fakeClient) GetBinaryInPage(u string, h map[string]string) (int, []byte, error) {
	if f.getBinIP == nil {
		return 0, nil, fmt.Errorf("fakeClient.GetBinaryInPage not scripted (url=%s)", u)
	}
	return f.getBinIP(u, h)
}

// collectErr returns an err-sink func plus a pointer to the messages it saw.
func collectErr() (func(string), *[]string) {
	var msgs []string
	return func(m string) { msgs = append(msgs, m) }, &msgs
}

func metaBody(dlURL string) string {
	b, _ := json.Marshal(map[string]any{"download_url": dlURL})
	return string(b)
}

// ---------------------------------------------------------------------------
// SaveDownloadURL
// ---------------------------------------------------------------------------

func TestSaveDownloadURLSuccess(t *testing.T) {
	noSleep(t)
	dir := filepath.Join(t.TempDir(), "files") // not yet created, MkdirAll must make it
	payload := []byte("hello-bytes")
	c := &fakeClient{getBin: func(string) (int, []byte, string, error) { return 200, payload, "text/plain", nil }}
	errFn, msgs := collectErr()

	if !SaveDownloadURL(c, "https://blob/x?sig=1", dir, "my file.txt", "label", errFn) {
		t.Fatalf("expected success; errs=%v", *msgs)
	}
	got, err := os.ReadFile(filepath.Join(dir, "my file.txt"))
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("content = %q, want %q", got, payload)
	}
	if len(*msgs) != 0 {
		t.Errorf("unexpected errors: %v", *msgs)
	}
}

func TestSaveDownloadURLHTTPError(t *testing.T) {
	noSleep(t)
	dir := t.TempDir()
	c := &fakeClient{getBin: func(string) (int, []byte, string, error) { return 403, []byte("no"), "", nil }}
	errFn, msgs := collectErr()

	if SaveDownloadURL(c, "https://blob/x", dir, "f.txt", "sandbox /mnt/data/f.txt", errFn) {
		t.Fatal("expected failure on HTTP 403")
	}
	if len(*msgs) != 1 || (*msgs)[0] != "sandbox /mnt/data/f.txt: download HTTP 403" {
		t.Errorf("err = %v, want download HTTP 403 message", *msgs)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("no file should have been written, found %d", len(entries))
	}
}

func TestSaveDownloadURLTransportError(t *testing.T) {
	noSleep(t)
	dir := t.TempDir()
	c := &fakeClient{getBin: func(string) (int, []byte, string, error) {
		return 0, nil, "", fmt.Errorf("connection refused")
	}}
	errFn, msgs := collectErr()

	if SaveDownloadURL(c, "https://blob/x", dir, "f.txt", "library abc (f.txt)", errFn) {
		t.Fatal("expected failure on transport error")
	}
	if len(*msgs) != 1 || !strings.HasPrefix((*msgs)[0], "library abc (f.txt): ") ||
		!strings.Contains((*msgs)[0], "connection refused") {
		t.Errorf("err = %v, want '{label}: {error}'", *msgs)
	}
}

func TestSaveDownloadURLCollisionRename(t *testing.T) {
	noSleep(t)
	dir := t.TempDir()
	// Pre-create the target under its sanitized name.
	existing := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := []byte("new-bytes")
	dlURL := "https://blob/report?sig=9"
	c := &fakeClient{getBin: func(string) (int, []byte, string, error) { return 200, payload, "", nil }}
	errFn, _ := collectErr()

	if !SaveDownloadURL(c, dlURL, dir, "report.pdf", "label", errFn) {
		t.Fatal("expected success")
	}
	// Original untouched.
	if b, _ := os.ReadFile(existing); string(b) != "old" {
		t.Errorf("original file was overwritten")
	}
	// New file uses the collision-suffix prefix.
	want := fmt.Sprintf("%d_report.pdf", collisionSuffix(dlURL))
	if b, err := os.ReadFile(filepath.Join(dir, want)); err != nil {
		t.Fatalf("collision-renamed file %q missing: %v", want, err)
	} else if string(b) != string(payload) {
		t.Errorf("collision file content = %q", b)
	}
}

// TestSaveDownloadURLViaRealGetBinary exercises the real out-of-page GET path
// (browser.GetBinary with an injected httptest client) end to end through
// SaveDownloadURL.
func TestSaveDownloadURLViaRealGetBinary(t *testing.T) {
	noSleep(t)
	payload := []byte{0x89, 0x50, 0x4e, 0x47, 0x01}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	client := srv.Client()
	c := &fakeClient{getBin: func(u string) (int, []byte, string, error) {
		return browser.GetBinary(client, u)
	}}
	dir := t.TempDir()
	errFn, msgs := collectErr()
	if !SaveDownloadURL(c, srv.URL+"/blob?sig=1", dir, "image.png", "label", errFn) {
		t.Fatalf("expected success; errs=%v", *msgs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "image.png")); string(b) != string(payload) {
		t.Errorf("content mismatch through real GetBinary path")
	}
}

// ---------------------------------------------------------------------------
// DownloadFiles
// ---------------------------------------------------------------------------

func TestDownloadFilesBothMetadataURLFallback(t *testing.T) {
	noSleep(t)
	dir := t.TempDir()
	first := browser.BaseURL + "/backend-api/files/file-ABCDEFGH/download"
	second := browser.BaseURL + "/backend-api/files/download/file-ABCDEFGH"
	c := &fakeClient{
		fetch: func(u string, _ map[string]string) (int, string, error) {
			switch u {
			case first:
				return 404, "", nil // forces fallback
			case second:
				return 200, metaBody("https://blob/f?sig=1"), nil
			}
			return 0, "", fmt.Errorf("unexpected url %s", u)
		},
		getBin: func(string) (int, []byte, string, error) { return 200, []byte("data"), "text/plain", nil },
	}
	errFn, msgs := collectErr()
	ok, failed := DownloadFiles(c, []FileRef{{ID: "file-ABCDEFGH", Name: "note.txt"}}, dir, nil, errFn)
	if ok != 1 || failed != 0 {
		t.Fatalf("ok=%d failed=%d, want 1/0; errs=%v", ok, failed, *msgs)
	}
	// Both metadata URLs must have been tried, in order.
	if len(c.fetchURLs) != 2 || c.fetchURLs[0] != first || c.fetchURLs[1] != second {
		t.Errorf("metadata URLs = %v", c.fetchURLs)
	}
	if _, err := os.Stat(filepath.Join(dir, "note.txt")); err != nil {
		t.Errorf("file not written: %v", err)
	}
}

func TestDownloadFilesNoDownloadURLExpired(t *testing.T) {
	noSleep(t)
	dir := t.TempDir()
	c := &fakeClient{
		fetch: func(string, map[string]string) (int, string, error) {
			return 404, `{"detail":"gone"}`, nil
		},
	}
	errFn, msgs := collectErr()
	ok, failed := DownloadFiles(c, []FileRef{{ID: "file-XXXXYYYY", Name: "old.bin"}}, dir, nil, errFn)
	if ok != 0 || failed != 1 {
		t.Fatalf("ok=%d failed=%d, want 0/1", ok, failed)
	}
	if len(*msgs) != 1 {
		t.Fatalf("errs = %v, want 1", *msgs)
	}
	m := (*msgs)[0]
	// HTTP status is the LAST metadata response (404 here, both empty-of-url).
	for _, want := range []string{
		"file file-XXXXYYYY (old.bin): no download_url",
		"(HTTP 404, likely expired)",
		`body="`, // %q repr of the snippet
	} {
		if !strings.Contains(m, want) {
			t.Errorf("err %q missing %q", m, want)
		}
	}
}

func TestDownloadFilesContentTypeExtensionGuessing(t *testing.T) {
	noSleep(t)
	cases := []struct {
		mime, ext string
	}{
		{"image/png", ".png"}, {"image/jpeg", ".jpg"},
		{"image/webp", ".webp"}, {"application/pdf", ".pdf"},
	}
	for _, tc := range cases {
		t.Run(tc.mime, func(t *testing.T) {
			dir := t.TempDir()
			c := &fakeClient{
				fetch:  func(string, map[string]string) (int, string, error) { return 200, metaBody("https://blob/f"), nil },
				getBin: func(string) (int, []byte, string, error) { return 200, []byte("x"), tc.mime + "; charset=binary", nil },
			}
			errFn, msgs := collectErr()
			// Name has no dot, so the extension must be appended.
			ok, failed := DownloadFiles(c, []FileRef{{ID: "file-12345678", Name: "noext"}}, dir, nil, errFn)
			if ok != 1 || failed != 0 {
				t.Fatalf("ok=%d failed=%d; errs=%v", ok, failed, *msgs)
			}
			if _, err := os.Stat(filepath.Join(dir, "noext"+tc.ext)); err != nil {
				t.Errorf("expected file noext%s: %v", tc.ext, err)
			}
		})
	}
}

func TestDownloadFilesNoExtensionGuessWhenNameHasDot(t *testing.T) {
	noSleep(t)
	dir := t.TempDir()
	c := &fakeClient{
		fetch:  func(string, map[string]string) (int, string, error) { return 200, metaBody("https://blob/f"), nil },
		getBin: func(string) (int, []byte, string, error) { return 200, []byte("x"), "image/png", nil },
	}
	errFn, _ := collectErr()
	DownloadFiles(c, []FileRef{{ID: "file-12345678", Name: "already.dat"}}, dir, nil, errFn)
	if _, err := os.Stat(filepath.Join(dir, "already.dat")); err != nil {
		t.Errorf("name with a dot must not get an extension: %v", err)
	}
}

// TestDownloadFilesSameNameDedup shows that when two refs in one call share a
// sanitized name, the second is skipped by AlreadyHave once the first has
// written the file — matching Python, where already_have runs before every
// download and prevents a second copy.
func TestDownloadFilesSameNameDedup(t *testing.T) {
	noSleep(t)
	dir := t.TempDir()
	c := &fakeClient{
		fetch:  func(string, map[string]string) (int, string, error) { return 200, metaBody("https://blob/f"), nil },
		getBin: func(string) (int, []byte, string, error) { return 200, []byte("data"), "text/plain", nil },
	}
	errFn, msgs := collectErr()
	refs := []FileRef{
		{ID: "file-AAAAAAAA", Name: "dup.txt"},
		{ID: "file-BBBBBBBB", Name: "dup.txt"},
	}
	ok, failed := DownloadFiles(c, refs, dir, nil, errFn)
	// Second ref: AlreadyHave is true (base name "dup.txt" already on disk from
	// the first), so Python skips it. Confirm only one file exists.
	if ok != 1 || failed != 0 {
		t.Fatalf("ok=%d failed=%d; errs=%v", ok, failed, *msgs)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "dup.txt" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("files = %v, want just dup.txt (second ref skipped by already_have)", names)
	}
}

// TestIdTail covers the collision-rename prefix helper (Python's fid[-8:]).
// The collision branch inside DownloadFiles itself is near-unreachable because
// AlreadyHave, which runs first with the same sanitized base, matches any
// same-name copy already on disk; the reachable collision rename is the one in
// SaveDownloadURL (TestSaveDownloadURLCollisionRename), which does not consult
// AlreadyHave. This matches the Python, whose download_files collision path is
// equally hard to reach.
func TestIdTail(t *testing.T) {
	cases := map[string]string{
		"file-ABCDEFGHIJ": "CDEFGHIJ", // last 8 runes
		"short":           "short",    // shorter than 8 -> whole
		"exactly8":        "exactly8",
	}
	for in, want := range cases {
		if got := idTail(in); got != want {
			t.Errorf("idTail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDownloadFilesAlreadyHaveSkip(t *testing.T) {
	noSleep(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "have.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &fakeClient{} // no methods scripted: any call is a failure
	errFn, msgs := collectErr()
	ok, failed := DownloadFiles(c, []FileRef{{ID: "file-11112222", Name: "have.txt"}}, dir, nil, errFn)
	if ok != 0 || failed != 0 {
		t.Fatalf("ok=%d failed=%d, want 0/0", ok, failed)
	}
	if len(c.fetchURLs) != 0 {
		t.Errorf("skipped ref must not hit the network: %v", c.fetchURLs)
	}
	if len(*msgs) != 0 {
		t.Errorf("no errors expected: %v", *msgs)
	}
}

func TestDownloadFilesDownloadHTTPError(t *testing.T) {
	noSleep(t)
	dir := t.TempDir()
	c := &fakeClient{
		fetch:  func(string, map[string]string) (int, string, error) { return 200, metaBody("https://blob/f"), nil },
		getBin: func(string) (int, []byte, string, error) { return 500, nil, "", nil },
	}
	errFn, msgs := collectErr()
	ok, failed := DownloadFiles(c, []FileRef{{ID: "file-33334444", Name: "f.bin"}}, dir, nil, errFn)
	if ok != 0 || failed != 1 {
		t.Fatalf("ok=%d failed=%d, want 0/1", ok, failed)
	}
	if len(*msgs) != 1 || (*msgs)[0] != "file file-33334444 (f.bin): download HTTP 500" {
		t.Errorf("err = %v", *msgs)
	}
}

// ---------------------------------------------------------------------------
// DownloadSandboxFiles
// ---------------------------------------------------------------------------

func TestDownloadSandboxFilesSkipExisting(t *testing.T) {
	noSleep(t)
	dir := t.TempDir()
	// The sanitized base name already on disk -> skipped without a fetch.
	if err := os.WriteFile(filepath.Join(dir, "out.csv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &fakeClient{} // any call is a failure
	errFn, _ := collectErr()
	ok, failed := DownloadSandboxFiles(c, "cid-1",
		[]SandboxRef{{MessageID: "m1", Path: "/mnt/data/out.csv"}}, dir, nil, errFn)
	if ok != 0 || failed != 0 {
		t.Fatalf("ok=%d failed=%d, want 0/0", ok, failed)
	}
	if len(c.fetchURLs) != 0 {
		t.Errorf("existing sandbox file must not be fetched: %v", c.fetchURLs)
	}
}

func TestDownloadSandboxFilesPercentEncoding(t *testing.T) {
	noSleep(t)
	dir := t.TempDir()
	// Goldens produced by python3: from urllib.parse import quote; quote(p, safe='')
	golden := map[string]string{
		"/mnt/data/foo bar.csv": "%2Fmnt%2Fdata%2Ffoo%20bar.csv",
		"/mnt/data/re+sült.txt": "%2Fmnt%2Fdata%2Fre%2Bs%C3%BClt.txt",
		"/mnt/data/日本語.csv":     "%2Fmnt%2Fdata%2F%E6%97%A5%E6%9C%AC%E8%AA%9E.csv",
	}
	var refs []SandboxRef
	paths := make([]string, 0, len(golden))
	for p := range golden {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		refs = append(refs, SandboxRef{MessageID: "m", Path: p})
	}
	c := &fakeClient{
		fetch: func(string, map[string]string) (int, string, error) { return 404, "", nil }, // no download_url -> failed, but URL recorded
	}
	errFn, _ := collectErr()
	DownloadSandboxFiles(c, "cid-1", refs, dir, nil, errFn)

	if len(c.fetchURLs) != len(paths) {
		t.Fatalf("fetched %d urls, want %d", len(c.fetchURLs), len(paths))
	}
	for i, p := range paths {
		u, err := url.Parse(c.fetchURLs[i])
		if err != nil {
			t.Fatalf("parsing %q: %v", c.fetchURLs[i], err)
		}
		// Compare the RAW encoded sandbox_path (url.Parse would decode it), so
		// pull it straight out of RawQuery.
		raw := u.RawQuery
		idx := strings.Index(raw, "sandbox_path=")
		if idx < 0 {
			t.Fatalf("no sandbox_path in %q", raw)
		}
		gotEnc := raw[idx+len("sandbox_path="):]
		if gotEnc != golden[p] {
			t.Errorf("path %q encoded as %q, want %q (python quote safe='')", p, gotEnc, golden[p])
		}
	}
}

func TestDownloadSandboxFilesNoDownloadURL(t *testing.T) {
	noSleep(t)
	dir := t.TempDir()
	c := &fakeClient{
		fetch: func(string, map[string]string) (int, string, error) { return 410, "", nil },
	}
	errFn, msgs := collectErr()
	ok, failed := DownloadSandboxFiles(c, "cid-1",
		[]SandboxRef{{MessageID: "m9", Path: "/mnt/data/gen.png"}}, dir, nil, errFn)
	if ok != 0 || failed != 1 {
		t.Fatalf("ok=%d failed=%d, want 0/1", ok, failed)
	}
	if len(*msgs) != 1 || (*msgs)[0] != "sandbox /mnt/data/gen.png (msg m9): no download_url (HTTP 410, likely expired)" {
		t.Errorf("err = %v", *msgs)
	}
}

func TestDownloadSandboxFilesSuccess(t *testing.T) {
	noSleep(t)
	dir := t.TempDir()
	c := &fakeClient{
		fetch:  func(string, map[string]string) (int, string, error) { return 200, metaBody("https://blob/gen"), nil },
		getBin: func(string) (int, []byte, string, error) { return 200, []byte("csvdata"), "text/csv", nil },
	}
	errFn, msgs := collectErr()
	ok, failed := DownloadSandboxFiles(c, "cid-1",
		[]SandboxRef{{MessageID: "m1", Path: "/mnt/data/result.csv"}}, dir, nil, errFn)
	if ok != 1 || failed != 0 {
		t.Fatalf("ok=%d failed=%d; errs=%v", ok, failed, *msgs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "result.csv")); string(b) != "csvdata" {
		t.Errorf("sandbox file content = %q", b)
	}
}

// ---------------------------------------------------------------------------
// FetchLibrary
// ---------------------------------------------------------------------------

func TestFetchLibraryPagination(t *testing.T) {
	noSleep(t)
	page := 0
	c := &fakeClient{
		postJSON: func(_ string, _ map[string]string, _ any) (int, string, error) {
			page++
			if page == 1 {
				return 200, `{"items":[{"file_id":"a"},{"file_id":"b"}],"cursor":"CUR"}`, nil
			}
			return 200, `{"items":[{"file_id":"c"}],"cursor":null}`, nil
		},
	}
	items, err := FetchLibrary(c, nil)
	if err != nil {
		t.Fatalf("FetchLibrary: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	// Second POST must carry the cursor from page one; first must be empty.
	if len(c.postBodies) != 2 {
		t.Fatalf("posted %d times, want 2", len(c.postBodies))
	}
	first, _ := c.postBodies[0].(map[string]any)
	if _, has := first["cursor"]; has {
		t.Errorf("first POST must not carry a cursor: %v", first)
	}
	second, _ := c.postBodies[1].(map[string]any)
	if second["cursor"] != "CUR" {
		t.Errorf("second POST cursor = %v, want CUR", second["cursor"])
	}
}

func TestFetchLibraryNumericCursorKeepsPaging(t *testing.T) {
	noSleep(t)
	// A numeric cursor token must be carried back and keep paging, not
	// silently truncate the sweep (Python posts the raw value regardless
	// of type). JSON numbers decode to float64.
	page := 0
	c := &fakeClient{
		postJSON: func(_ string, _ map[string]string, _ any) (int, string, error) {
			page++
			if page == 1 {
				return 200, `{"items":[{"file_id":"a"}],"cursor":5}`, nil
			}
			return 200, `{"items":[{"file_id":"b"}],"cursor":0}`, nil
		},
	}
	items, err := FetchLibrary(c, nil)
	if err != nil {
		t.Fatalf("FetchLibrary: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (numeric cursor must not truncate)", len(items))
	}
	if len(c.postBodies) != 2 {
		t.Fatalf("posted %d times, want 2", len(c.postBodies))
	}
	second, _ := c.postBodies[1].(map[string]any)
	if second["cursor"] != float64(5) {
		t.Errorf("second POST cursor = %v (%T), want 5", second["cursor"], second["cursor"])
	}
}

func TestFetchLibraryNon200Breaks(t *testing.T) {
	noSleep(t)
	c := &fakeClient{
		postJSON: func(string, map[string]string, any) (int, string, error) { return 500, "boom", nil },
	}
	items, err := FetchLibrary(c, nil)
	if err != nil {
		t.Fatalf("non-200 should break, not error: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items = %v, want empty", items)
	}
	if len(c.postBodies) != 1 {
		t.Errorf("posted %d times, want 1 (break after non-200)", len(c.postBodies))
	}
}

// ---------------------------------------------------------------------------
// LibrarySweep
// ---------------------------------------------------------------------------

func TestLibrarySweepFolderRoutingAndFallback(t *testing.T) {
	noSleep(t)
	out := t.TempDir()
	cid := "11111111-2222-3333-4444-555555555555"
	// manifest.csv: url, title, folder
	manifest := "https://chatgpt.com/c/" + cid + ",My Chat,My Chat 2024\n"
	if err := os.WriteFile(filepath.Join(out, "manifest.csv"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-create the chat folder so os.MkdirAll(folder/files) is a real subdir.
	if err := os.MkdirAll(filepath.Join(out, "My Chat 2024"), 0o755); err != nil {
		t.Fatal(err)
	}

	items := []map[string]any{
		{"file_id": "f1", "file_name": "routed.png", "origination_thread_id": cid},
		{"file_id": "f2", "file_name": "orphan.png", "origination_thread_id": "unknown-tid"},
	}
	itemsJSON, _ := json.Marshal(map[string]any{"items": items, "cursor": nil})
	c := &fakeClient{
		postJSON: func(string, map[string]string, any) (int, string, error) { return 200, string(itemsJSON), nil },
		fetch:    func(string, map[string]string) (int, string, error) { return 200, metaBody("https://blob/x"), nil },
		getBin:   func(string) (int, []byte, string, error) { return 200, []byte("img"), "image/png", nil },
	}
	errFn, msgs := collectErr()
	LibrarySweep(c, out, nil, errFn)
	if len(*msgs) != 0 {
		t.Errorf("unexpected errors: %v", *msgs)
	}
	// Routed into the chat's files/ folder.
	if _, err := os.Stat(filepath.Join(out, "My Chat 2024", "files", "routed.png")); err != nil {
		t.Errorf("routed file missing: %v", err)
	}
	// Orphan lands in _library/.
	if _, err := os.Stat(filepath.Join(out, "_library", "orphan.png")); err != nil {
		t.Errorf("orphan file missing: %v", err)
	}
}

func TestLibrarySweepSkipExisting(t *testing.T) {
	noSleep(t)
	out := t.TempDir()
	// No manifest -> everything routes to _library.
	if err := os.MkdirAll(filepath.Join(out, "_library"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "_library", "present.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	items := []map[string]any{{"file_id": "f1", "file_name": "present.png"}}
	itemsJSON, _ := json.Marshal(map[string]any{"items": items})
	c := &fakeClient{
		postJSON: func(string, map[string]string, any) (int, string, error) { return 200, string(itemsJSON), nil },
		// fetch/getBin unscripted: a skip must not call them.
	}
	errFn, msgs := collectErr()
	LibrarySweep(c, out, nil, errFn)
	if len(c.fetchURLs) != 0 {
		t.Errorf("existing library file must be skipped, not fetched: %v", c.fetchURLs)
	}
	if len(*msgs) != 0 {
		t.Errorf("no errors expected: %v", *msgs)
	}
}

// ---------------------------------------------------------------------------
// FixFiles
// ---------------------------------------------------------------------------

func writeConversation(t *testing.T, dir string, conv map[string]any) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(conv)
	if err := os.WriteFile(filepath.Join(dir, "conversation.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFixFilesEndToEnd(t *testing.T) {
	noSleep(t)
	out := t.TempDir()

	// Folder A: one attachment (missing) + one sandbox file (missing).
	convA := map[string]any{
		"conversation_id": "aaaaaaaa-1111-2222-3333-444444444444",
		"mapping": map[string]any{
			"n1": map[string]any{"message": map[string]any{
				"id":       "m1",
				"metadata": map[string]any{"attachments": []any{map[string]any{"id": "file-ATTACH01", "name": "doc.pdf"}}},
				"content":  map[string]any{"content_type": "text", "parts": []any{"see sandbox:/mnt/data/plot.png here"}},
			}},
		},
	}
	writeConversation(t, filepath.Join(out, "Chat A"), convA)

	// Folder B: one attachment that is ALREADY present -> nothing to do.
	convB := map[string]any{
		"conversation_id": "bbbbbbbb-1111-2222-3333-444444444444",
		"mapping": map[string]any{
			"n1": map[string]any{"message": map[string]any{
				"id":       "m2",
				"metadata": map[string]any{"attachments": []any{map[string]any{"id": "file-HAVE0001", "name": "already.txt"}}},
				"content":  map[string]any{"parts": []any{"hi"}},
			}},
		},
	}
	writeConversation(t, filepath.Join(out, "Chat B"), convB)
	// Pre-create Chat B's file so already_have skips it.
	if err := os.MkdirAll(filepath.Join(out, "Chat B", "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "Chat B", "files", "already.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A non-directory and a dir without conversation.json must be ignored.
	if err := os.WriteFile(filepath.Join(out, "loose.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(out, "NotAChat"), 0o755); err != nil {
		t.Fatal(err)
	}

	sandboxURL := browser.BaseURL + "/backend-api/conversation/"
	c := &fakeClient{
		fetch: func(u string, _ map[string]string) (int, string, error) {
			// Attachment metadata and sandbox metadata both return a url.
			if strings.Contains(u, "/interpreter/download") {
				return 200, metaBody("https://blob/sandbox"), nil
			}
			return 200, metaBody("https://blob/attach"), nil
		},
		getBin: func(u string) (int, []byte, string, error) { return 200, []byte("bytes"), "application/pdf", nil },
	}
	var records []string
	ef := func(s string) { records = append(records, s) }

	FixFiles(c, out, nil, ef)

	// Chat A downloads: attachment doc.pdf and sandbox plot.png.
	if _, err := os.Stat(filepath.Join(out, "Chat A", "files", "doc.pdf")); err != nil {
		t.Errorf("Chat A attachment not downloaded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "Chat A", "files", "plot.png")); err != nil {
		t.Errorf("Chat A sandbox file not downloaded: %v", err)
	}
	// Chat B: nothing new written beyond the pre-existing file.
	entriesB, _ := os.ReadDir(filepath.Join(out, "Chat B", "files"))
	if len(entriesB) != 1 {
		t.Errorf("Chat B files = %d, want 1 (already present)", len(entriesB))
	}
	if len(records) != 0 {
		t.Errorf("unexpected error records: %v", records)
	}
	_ = sandboxURL
}

func TestFixFilesErrorRecordFormat(t *testing.T) {
	noSleep(t)
	out := t.TempDir()
	conv := map[string]any{
		"conversation_id": "cccccccc-1111-2222-3333-444444444444",
		"mapping": map[string]any{
			"n1": map[string]any{"message": map[string]any{
				"id":       "m1",
				"metadata": map[string]any{"attachments": []any{map[string]any{"id": "file-GONE0001", "name": "lost.bin"}}},
				"content":  map[string]any{"parts": []any{"x"}},
			}},
		},
	}
	writeConversation(t, filepath.Join(out, "Chat C"), conv)
	c := &fakeClient{
		fetch: func(string, map[string]string) (int, string, error) { return 404, "", nil }, // no download_url
	}
	var records []string
	FixFiles(c, out, nil, func(s string) { records = append(records, s) })

	if len(records) != 1 {
		t.Fatalf("records = %v, want 1", records)
	}
	want := "[fix-files Chat C]\n    file file-GONE0001 (lost.bin): no download_url"
	if !strings.HasPrefix(records[0], want) {
		t.Errorf("record = %q, want prefix %q", records[0], want)
	}
}

// TestFixFilesCidFromMarkdown covers the fallback where conversation.json has
// no conversation_id and the id is recovered from conversation.md[:500].
func TestFixFilesCidFromMarkdown(t *testing.T) {
	noSleep(t)
	out := t.TempDir()
	cid := "dddddddd-1111-2222-3333-444444444444"
	dir := filepath.Join(out, "Chat D")
	conv := map[string]any{
		// no conversation_id
		"mapping": map[string]any{
			"n1": map[string]any{"message": map[string]any{
				"id":      "m1",
				"content": map[string]any{"parts": []any{"gen sandbox:/mnt/data/g.csv"}},
			}},
		},
	}
	writeConversation(t, dir, conv)
	md := "# Title\nsource: https://chatgpt.com/c/" + cid + "\n"
	if err := os.WriteFile(filepath.Join(dir, "conversation.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	var sandboxCID string
	c := &fakeClient{
		fetch: func(u string, _ map[string]string) (int, string, error) {
			if strings.Contains(u, "/conversation/"+cid+"/interpreter/download") {
				sandboxCID = cid
			}
			return 200, metaBody("https://blob/g"), nil
		},
		getBin: func(string) (int, []byte, string, error) { return 200, []byte("d"), "text/csv", nil },
	}
	FixFiles(c, out, nil, func(string) {})
	if sandboxCID != cid {
		t.Errorf("sandbox download did not use the cid recovered from markdown (got %q)", sandboxCID)
	}
	if _, err := os.Stat(filepath.Join(dir, "files", "g.csv")); err != nil {
		t.Errorf("sandbox file not downloaded: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers: percent-encoding goldens and collision suffix
// ---------------------------------------------------------------------------

func TestPyQuoteGoldens(t *testing.T) {
	// Verified against python3: from urllib.parse import quote; quote(p, safe='')
	cases := map[string]string{
		"/mnt/data/foo bar.csv":      "%2Fmnt%2Fdata%2Ffoo%20bar.csv",
		"/mnt/data/re+sült.txt":      "%2Fmnt%2Fdata%2Fre%2Bs%C3%BClt.txt",
		"/mnt/data/a/b c.png":        "%2Fmnt%2Fdata%2Fa%2Fb%20c.png",
		"/mnt/data/100%done.txt":     "%2Fmnt%2Fdata%2F100%25done.txt",
		"/mnt/data/tab\tend.txt":     "%2Fmnt%2Fdata%2Ftab%09end.txt",
		"/mnt/data/日本語.csv":          "%2Fmnt%2Fdata%2F%E6%97%A5%E6%9C%AC%E8%AA%9E.csv",
		"/mnt/data/tilde~und_er.dat": "%2Fmnt%2Fdata%2Ftilde~und_er.dat",
		"/mnt/data/dash-dot.a.b":     "%2Fmnt%2Fdata%2Fdash-dot.a.b",
	}
	for in, want := range cases {
		if got := pyQuote(in); got != want {
			t.Errorf("pyQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCollisionSuffixDeterministicAndBounded(t *testing.T) {
	a := collisionSuffix("https://blob/x?sig=1")
	b := collisionSuffix("https://blob/x?sig=1")
	if a != b {
		t.Errorf("collisionSuffix not deterministic: %d vs %d", a, b)
	}
	if a < 0 || a >= 99999 {
		t.Errorf("collisionSuffix out of range: %d", a)
	}
}

func TestBaseName(t *testing.T) {
	cases := map[string]string{
		"/mnt/data/x.csv": "x.csv",
		"plain":           "plain",
		"a/b/c":           "c",
	}
	for in, want := range cases {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Confirm the adapter compiles against a real (nil) session type without
// requiring a browser; NewSessionClient must return a non-nil Client.
func TestNewSessionClientReturnsAdapter(t *testing.T) {
	var s *browser.Session
	c := NewSessionClient(s)
	if c == nil {
		t.Fatal("NewSessionClient returned nil")
	}
}
