package files

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/saigyo/scrapemychats/internal/browser"
	"github.com/saigyo/scrapemychats/internal/discover"
	"github.com/saigyo/scrapemychats/internal/fsutil"
)

// Client is the network seam the download functions run against. It gathers
// the four primitives export_chats.py reaches for while recovering files, so
// the porting logic can be exercised with a scripted fake AND, for the
// out-of-page GET, against an httptest.Server. *browser.Session satisfies it
// through NewSessionClient.
//
//   - Fetch is an in-page session-authenticated GET, used for the /download
//     metadata endpoints that return JSON with a download_url.
//   - PostJSON is an in-page session-authenticated POST, used for the Library
//     listing.
//   - GetBinary is an out-of-page GET of a pre-signed URL returning raw bytes.
//   - GetBinaryInPage is an in-page fetch of the bytes, available as a fallback
//     for a URL that needs the browsing session (cookies) rather than a
//     pre-signed signature. The current download paths only ever hit
//     pre-signed URLs, so this is not wired in — matching the Python, which
//     likewise has no in-page download fallback (see browser.GetBinaryInPage).
type Client interface {
	Fetch(url string, headers map[string]string) (status int, body string, err error)
	PostJSON(url string, headers map[string]string, body any) (status int, body2 string, err error)
	GetBinary(url string) (status int, data []byte, contentType string, err error)
	GetBinaryInPage(url string, headers map[string]string) (status int, data []byte, err error)
}

// sleep is time.Sleep behind a package variable so tests run the download
// loops without waiting. It mirrors the sleep seam in internal/browser.
var sleep = time.Sleep

// delay pauses for a uniformly random duration in [minS, maxS] seconds,
// matching Python's time.sleep(random.uniform(minS, maxS)).
func delay(minS, maxS float64) {
	sleep(time.Duration((minS + rand.Float64()*(maxS-minS)) * float64(time.Second)))
}

// sessionClient adapts a *browser.Session to Client. It owns one
// *http.Client for the out-of-page GetBinary path; the download URLs are
// pre-signed, so that client carries no session/cookies (see browser.GetBinary).
type sessionClient struct {
	s    *browser.Session
	http *http.Client
}

// NewSessionClient wraps a live browser session so the download functions can
// drive it. The out-of-page GetBinary path gets its own http.Client from
// browser.DefaultDownloadClient.
func NewSessionClient(s *browser.Session) Client {
	return &sessionClient{s: s, http: browser.DefaultDownloadClient()}
}

func (c *sessionClient) Fetch(u string, h map[string]string) (int, string, error) {
	return browser.FetchWithSession(c.s, u, h)
}

func (c *sessionClient) PostJSON(u string, h map[string]string, body any) (int, string, error) {
	return browser.PostJSON(c.s, u, h, body)
}

func (c *sessionClient) GetBinary(u string) (int, []byte, string, error) {
	return browser.GetBinary(c.http, u)
}

func (c *sessionClient) GetBinaryInPage(u string, h map[string]string) (int, []byte, error) {
	return browser.GetBinaryInPage(c.s, u, h)
}

// collisionSuffix returns a deterministic 0..99998 number derived from s,
// standing in for Python's `abs(hash(dl_url)) % 99999`. Python's hash() is
// salted and platform-dependent, so an identical value is impossible and also
// pointless: the number only disambiguates the rare on-disk filename
// collision, it never affects which bytes are written or whether a download
// succeeds. FNV-1a gives a stable, well-distributed stand-in.
func collisionSuffix(s string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return int(h.Sum32() % 99999)
}

// pyQuote percent-encodes s exactly like Python's urllib.parse.quote with an
// empty safe set: every character except the unreserved set (A-Z a-z 0-9 and
// -_.~) becomes %XX, and — crucially — a space becomes %20, not a plus.
// url.QueryEscape already matches for slash, plus, unicode and control bytes;
// its one divergence is the space, which it renders as a plus, so that is
// rewritten here. Because QueryEscape encodes a literal plus as %2B, no plus in
// its output ever means anything but a space, making the replacement safe.
// Verified against python3 quote(p, safe=empty) for space, plus, slash,
// unicode, percent, tab and tilde inputs.
func pyQuote(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// baseName returns the substring after the last '/', matching Python's
// path.rsplit("/", 1)[-1] (filepath.Base differs on a trailing slash, which
// sandbox paths never carry, but rsplit semantics are reproduced exactly).
func baseName(p string) string {
	return p[strings.LastIndex(p, "/")+1:]
}

// fileExists reports whether a plain filesystem entry exists at path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// downloadURLOf parses a /download metadata body and returns its download_url.
// An empty body (or a non-200 status the caller already folded into "") yields
// "" with no error; malformed JSON returns the parse error so the caller can
// mirror Python's json.loads raising.
func downloadURLOf(status int, body string) (string, error) {
	if status != 200 || body == "" {
		return "", nil
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		return "", err
	}
	s, _ := info["download_url"].(string)
	return s, nil
}

// SaveDownloadURL fetches a pre-signed URL and writes it under filesDir. Port
// of save_download_url (export_chats.py:426-436).
//
// On any failure it reports via err and returns false; success returns true.
// The label prefixes the error message, and — matching the shape of Python's
// callers, whose surrounding try/except emits "{label}: {e}" — a transport or
// write error is reported as "{label}: {error}", while a non-200 download is
// "{label}: download HTTP {status}", exactly the message save_download_url
// itself produces.
func SaveDownloadURL(c Client, dlURL, filesDir, name, label string, err func(string)) bool {
	status, data, _, e := c.GetBinary(dlURL)
	if e != nil {
		err(fmt.Sprintf("%s: %v", label, e))
		return false
	}
	if status != 200 {
		err(fmt.Sprintf("%s: download HTTP %d", label, status))
		return false
	}
	if e := os.MkdirAll(filesDir, 0o755); e != nil {
		err(fmt.Sprintf("%s: %v", label, e))
		return false
	}
	sanitized := fsutil.SanitizeN(name, 100)
	target := filepath.Join(filesDir, sanitized)
	if fileExists(target) {
		target = filepath.Join(filesDir, fmt.Sprintf("%d_%s", collisionSuffix(dlURL), sanitized))
	}
	if e := os.WriteFile(target, data, 0o644); e != nil {
		err(fmt.Sprintf("%s: %v", label, e))
		return false
	}
	return true
}

// DownloadSandboxFiles downloads code-tool output files through the
// interpreter/download route. Port of download_sandbox_files
// (export_chats.py:439-467).
func DownloadSandboxFiles(c Client, cid string, refs []SandboxRef, filesDir string, auth map[string]string, err func(string)) (ok, failed int) {
	for _, ref := range refs {
		name := baseName(ref.Path)
		if fileExists(filepath.Join(filesDir, fsutil.SanitizeN(name, 100))) {
			continue
		}
		api := fmt.Sprintf("%s/backend-api/conversation/%s/interpreter/download?message_id=%s&sandbox_path=%s",
			browser.BaseURL, cid, ref.MessageID, pyQuote(ref.Path))
		status, body, e := browser.APIGet(c, api, auth)
		if e != nil {
			err(fmt.Sprintf("sandbox %s: %v", ref.Path, e))
			failed++
			delay(1.5, 3.0)
			continue
		}
		dlURL, e := downloadURLOf(status, body)
		if e != nil {
			err(fmt.Sprintf("sandbox %s: %v", ref.Path, e))
			failed++
			delay(1.5, 3.0)
			continue
		}
		if dlURL == "" {
			err(fmt.Sprintf("sandbox %s (msg %s): no download_url (HTTP %d, likely expired)",
				ref.Path, ref.MessageID, status))
			failed++
			// Python `continue` here skips the trailing time.sleep.
			continue
		}
		if SaveDownloadURL(c, dlURL, filesDir, name, "sandbox "+ref.Path, err) {
			ok++
		} else {
			failed++
		}
		delay(1.5, 3.0)
	}
	return ok, failed
}

// FetchLibrary pages through the account file Library. Port of fetch_library
// (export_chats.py:470-496). It returns whatever it accumulated together with
// the first error that stopped it early (a browser-eval failure or a body
// that would not parse — both of which Python would let propagate).
func FetchLibrary(c Client, auth map[string]string) ([]map[string]any, error) {
	items := []map[string]any{}
	var cursor any // nil == Python's None
	for {
		var body any
		// Python: {"cursor": cursor} if cursor else {} — carries the cursor
		// back verbatim whatever its JSON type (string or numeric token).
		if cursorTruthy(cursor) {
			body = map[string]any{"cursor": cursor}
		} else {
			body = map[string]any{}
		}
		status, respBody, err := c.PostJSON(browser.BaseURL+"/backend-api/files/library", auth, body)
		if err != nil {
			return items, err
		}
		if status != 200 {
			fsutil.Log(fmt.Sprintf("  ! library listing failed (HTTP %d)", status))
			break
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(respBody), &data); err != nil {
			return items, err
		}
		batch := mapSlice(data["items"]) // Python: data.get("items") or []
		items = append(items, batch...)
		cursor = data["cursor"]
		if len(batch) == 0 || !cursorTruthy(cursor) {
			break
		}
		delay(1, 2)
	}
	return items, nil
}

// cursorTruthy reports whether a JSON-decoded pagination cursor is "truthy"
// in Python's sense (nil, "", and 0 are falsy), matching fetch_library's
// `{"cursor": cursor} if cursor else {}` and its `if not cursor: break`
// (export_chats.py:475,493). A numeric cursor token must keep paging rather
// than silently truncate the sweep, so this mirrors discover.cursorTruthy.
func cursorTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case float64:
		return x != 0
	case bool:
		return x
	}
	return true
}

// mapSlice converts a decoded JSON array into []map[string]any, dropping any
// element that is not an object. A nil or non-array value yields an empty
// slice, mirroring Python's `... or []`.
func mapSlice(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// LibrarySweep downloads every Library file into the folder of the chat it
// came from (or outDir/_library when that chat was not exported). Port of
// library_sweep (export_chats.py:499-549).
func LibrarySweep(c Client, outDir string, auth map[string]string, err func(string)) {
	fsutil.Log("Sweeping the account file Library...")
	items, e := FetchLibrary(c, auth)
	if e != nil {
		// Python has no such guard (fetch_library would raise and abort the
		// sweep); here the sweep continues with whatever was collected so a
		// single flaky page does not lose the rest.
		fsutil.Log(fmt.Sprintf("  ! library listing error: %v", e))
	}
	fsutil.Log(fmt.Sprintf("  %d files in library", len(items)))

	folderByCID := readLibraryManifest(outDir)

	ok, failed, skipped := 0, 0, 0
	for _, it := range items {
		fid, _ := it["file_id"].(string)
		name, _ := it["file_name"].(string)
		if name == "" { // Python: it.get("file_name") or it.get("file_id")
			name = fid
		}
		tid, _ := it["origination_thread_id"].(string)
		if fid == "" {
			continue
		}
		var filesDir string
		if folder, in := folderByCID[tid]; in {
			filesDir = filepath.Join(outDir, folder, "files")
		} else {
			filesDir = filepath.Join(outDir, "_library")
		}
		if fileExists(filepath.Join(filesDir, fsutil.SanitizeN(name, 100))) {
			skipped++
			continue
		}
		status, body, e := browser.APIGet(c, browser.BaseURL+"/backend-api/files/"+fid+"/download", auth)
		if e != nil {
			err(fmt.Sprintf("library %s (%s): %v", fid, name, e))
			failed++
			delay(1.5, 3.0)
			continue
		}
		dlURL, e := downloadURLOf(status, body)
		if e != nil {
			err(fmt.Sprintf("library %s (%s): %v", fid, name, e))
			failed++
			delay(1.5, 3.0)
			continue
		}
		if dlURL == "" {
			err(fmt.Sprintf("library %s (%s): no download_url (HTTP %d)", fid, name, status))
			failed++
			// Python `continue` skips the trailing time.sleep.
			continue
		}
		if e := os.MkdirAll(filesDir, 0o755); e != nil {
			err(fmt.Sprintf("library %s (%s): %v", fid, name, e))
			failed++
			delay(1.5, 3.0)
			continue
		}
		if SaveDownloadURL(c, dlURL, filesDir, name, fmt.Sprintf("library %s (%s)", fid, name), err) {
			ok++
		} else {
			failed++
		}
		delay(1.5, 3.0)
	}
	fsutil.Log(fmt.Sprintf("  library sweep: %d downloaded, %d already present, %d failed", ok, skipped, failed))
}

// readLibraryManifest maps a full conversation id to its exported folder using
// out_dir/manifest.csv, mirroring the folder_by_cid build in library_sweep
// (export_chats.py:509-517). The manifest is read as plain UTF-8 (no BOM
// handling — Python opens it with encoding="utf-8"), ragged rows allowed. A
// missing manifest yields an empty map.
func readLibraryManifest(outDir string) map[string]string {
	folderByCID := map[string]string{}
	f, err := os.Open(filepath.Join(outDir, "manifest.csv"))
	if err != nil {
		return folderByCID
	}
	defer f.Close()
	r := plainCSVReader(f)
	for {
		row, rerr := r.Read()
		if rerr != nil {
			break
		}
		if len(row) >= 3 && row[2] != "" {
			if m := discover.ConvIDRE.FindStringSubmatch(row[0]); m != nil {
				folderByCID[m[1]] = row[2]
			}
		}
	}
	return folderByCID
}

// plainCSVReader returns a csv.Reader that accepts ragged rows, matching
// Python's csv.reader over a plain UTF-8 file (manifest.csv is read WITHOUT
// BOM handling, unlike chats.csv).
func plainCSVReader(r io.Reader) *csv.Reader {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	return cr
}

// DownloadFiles downloads the uploaded/attached files a conversation refers
// to. Port of download_files (export_chats.py:565-620).
func DownloadFiles(c Client, refs []FileRef, filesDir string, auth map[string]string, err func(string)) (ok, failed int) {
	for _, ref := range refs {
		if AlreadyHave(filesDir, ref.Name, ref.ID) {
			continue
		}
		downloaded, doSleep := downloadOneFile(c, ref, filesDir, auth, err)
		if downloaded {
			ok++
		} else {
			failed++
		}
		if doSleep {
			delay(1.5, 3.0)
		}
	}
	return ok, failed
}

// downloadOneFile handles a single ref for DownloadFiles. It returns whether
// the file was downloaded and whether the caller should then sleep — Python's
// `continue` after the "no download_url" and "download HTTP" errors skips the
// trailing time.sleep, whereas success and any exception reach it.
func downloadOneFile(c Client, ref FileRef, filesDir string, auth map[string]string, err func(string)) (downloaded, doSleep bool) {
	fid, name := ref.ID, ref.Name

	var info map[string]any
	haveInfo := false
	lastStatus := 0
	lastBody := ""
	for _, api := range []string{
		browser.BaseURL + "/backend-api/files/" + fid + "/download",
		browser.BaseURL + "/backend-api/files/download/" + fid,
	} {
		status, body, e := browser.APIGet(c, api, auth)
		if e != nil {
			err(fmt.Sprintf("file %s (%s): %v", fid, name, e))
			return false, true // exception path -> Python reaches time.sleep
		}
		lastStatus, lastBody = status, body
		if status == 200 && body != "" {
			if e := json.Unmarshal([]byte(body), &info); e != nil {
				// Python's json.loads would raise into the except.
				err(fmt.Sprintf("file %s (%s): %v", fid, name, e))
				return false, true
			}
			haveInfo = true
			break
		}
	}

	dlURL := ""
	if haveInfo {
		dlURL, _ = info["download_url"].(string)
	}
	if dlURL == "" {
		// Very common: OpenAI deletes old file content server-side. The
		// conversation text is still captured; only the file is gone.
		snippet := lastBody
		if r := []rune(snippet); len(r) > 200 {
			snippet = string(r[:200])
		}
		err(fmt.Sprintf("file %s (%s): no download_url (HTTP %s, likely expired) body=%q",
			fid, name, strconv.Itoa(lastStatus), snippet))
		return false, false // Python `continue` skips the sleep
	}

	status, data, ctype, e := c.GetBinary(dlURL)
	if e != nil {
		err(fmt.Sprintf("file %s (%s): %v", fid, name, e))
		return false, true // exception path
	}
	if status != 200 {
		err(fmt.Sprintf("file %s (%s): download HTTP %d", fid, name, status))
		return false, false // Python `continue` skips the sleep
	}

	fname := fsutil.SanitizeN(name, 100)
	if !strings.Contains(fname, ".") {
		for _, me := range []struct{ mime, ext string }{
			{"image/png", ".png"}, {"image/jpeg", ".jpg"},
			{"image/webp", ".webp"}, {"application/pdf", ".pdf"},
		} {
			if strings.Contains(ctype, me.mime) {
				fname += me.ext
				break
			}
		}
	}
	if e := os.MkdirAll(filesDir, 0o755); e != nil {
		err(fmt.Sprintf("file %s (%s): %v", fid, name, e))
		return false, true
	}
	target := filepath.Join(filesDir, fname)
	if fileExists(target) {
		target = filepath.Join(filesDir, idTail(fid)+"_"+fname)
	}
	if e := os.WriteFile(target, data, 0o644); e != nil {
		err(fmt.Sprintf("file %s (%s): %v", fid, name, e))
		return false, true
	}
	return true, true
}

// idTail returns the last 8 characters (runes) of fid, matching Python's
// fid[-8:] used for the collision-rename prefix. A shorter id is returned
// whole.
func idTail(fid string) string {
	r := []rune(fid)
	if len(r) > 8 {
		return string(r[len(r)-8:])
	}
	return fid
}

// FixFiles revisits every exported chat folder and downloads whatever files
// are still missing: stale-id attachments (retried) and code-tool sandbox
// files. Port of fix_files (export_chats.py:623-662).
//
// ef receives a fully-formatted error record (the same text Python wrote to
// its error-log file): "[fix-files {folder}]\n    {msg}\n".
func FixFiles(c Client, outDir string, auth map[string]string, ef func(string)) {
	entries, e := os.ReadDir(outDir) // ReadDir returns entries sorted by name
	if e != nil {
		fsutil.Log(fmt.Sprintf("fix-files: cannot read %s: %v", outDir, e))
		return
	}
	var folders []string
	for _, entry := range entries {
		if entry.IsDir() && fileExists(filepath.Join(outDir, entry.Name(), "conversation.json")) {
			folders = append(folders, entry.Name())
		}
	}
	fsutil.Log(fmt.Sprintf("fix-files: checking %d exported chats for missing files", len(folders)))

	totOK, totFail := 0, 0
	for i, folderName := range folders {
		folder := filepath.Join(outDir, folderName)
		raw, e := os.ReadFile(filepath.Join(folder, "conversation.json"))
		if e != nil {
			fsutil.Log(fmt.Sprintf("  ! unreadable conversation.json in %s: %v", folderName, e))
			continue
		}
		conv, order, e := ParseWithOrder(raw)
		if e != nil {
			fsutil.Log(fmt.Sprintf("  ! unreadable conversation.json in %s: %v", folderName, e))
			continue
		}
		cid, _ := conv["conversation_id"].(string)
		if cid == "" {
			if md, e := os.ReadFile(filepath.Join(folder, "conversation.md")); e == nil {
				head := []rune(string(md))
				if len(head) > 500 {
					head = head[:500]
				}
				if m := discover.ConvIDRE.FindStringSubmatch(string(head)); m != nil {
					cid = m[1]
				}
			}
		}

		fname := folderName // capture for the closure
		errFn := func(msg string) {
			ef(fmt.Sprintf("[fix-files %s]\n    %s\n", fname, msg))
		}

		filesDir := filepath.Join(folder, "files")
		var refs []FileRef
		for _, r := range CollectFileRefs(conv, order) {
			if !AlreadyHave(filesDir, r.Name, r.ID) {
				refs = append(refs, r)
			}
		}
		var srefs []SandboxRef
		for _, s := range CollectSandboxRefs(conv, order) {
			if !AlreadyHave(filesDir, baseName(s.Path), "") {
				srefs = append(srefs, s)
			}
		}
		if len(refs) == 0 && len(srefs) == 0 {
			continue
		}
		fsutil.Log(fmt.Sprintf("[%d/%d] %s: %d attachment(s) + %d generated file(s) missing",
			i+1, len(folders), folderName, len(refs), len(srefs)))
		ok, fail := DownloadFiles(c, refs, filesDir, auth, errFn)
		totOK += ok
		totFail += fail
		if cid != "" && len(srefs) > 0 {
			ok, fail = DownloadSandboxFiles(c, cid, srefs, filesDir, auth, errFn)
			totOK += ok
			totFail += fail
		}
	}
	fsutil.Log(fmt.Sprintf("fix-files: recovered %d files, %d still unavailable", totOK, totFail))
}
