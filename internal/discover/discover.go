// Package discover finds every conversation in a ChatGPT account — both the
// ones in the main sidebar and the ones that live inside Projects (gizmos) —
// and turns them into the chats.csv the rest of the export pipeline reads.
// It is a behavioral port of export_chats.py's discover_main,
// discover_projects, discover_chats and read_chat_list
// (export_chats.py:149-286).
//
// Every function here takes a browser.Fetcher rather than a concrete
// *browser.Session, so the HTTP-shaped API traffic can be exercised in tests
// with a scripted fake instead of a real Chrome instance.
//
// Auth seam: Python's discover_chats calls capture_auth(page) itself
// (export_chats.py:236) before discovering. DiscoverChats here instead takes
// an already-captured headers map — capturing auth means driving real
// browser navigation and waiting for a specific network response
// (browser.CaptureAuth), which has no place in a fake-Fetcher unit test.
// The caller (the export entry point, wired up in a later task) is expected
// to call browser.CaptureAuth once and pass the resulting headers through
// DiscoverChats, DiscoverMain and DiscoverProjects alike.
package discover

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/saigyo/scrapemychats/internal/browser"
	"github.com/saigyo/scrapemychats/internal/fsutil"
)

// ConvIDRE extracts a conversation id from a chatgpt.com URL. It matches
// both "/c/{id}" and "/g/{gid}/c/{id}" because it is a plain substring
// search, not an anchored match — the same behavior as Python's
// CONV_ID_RE.search() (export_chats.py:43-45).
var ConvIDRE = regexp.MustCompile(
	`/c/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`,
)

// Chat is one discovered (or read-back) conversation.
type Chat struct {
	URL   string
	ID    string
	Title string
}

// sleep is time.Sleep, indirected through a package variable so tests can
// run the pagination loops without actually waiting (mirrors the same
// pattern in internal/browser).
var sleep = func(d time.Duration) { time.Sleep(d) }

// jitter returns a random duration in [min, max) seconds, matching Python's
// time.sleep(random.uniform(min, max)).
func jitter(min, max float64) time.Duration {
	return time.Duration((min + rand.Float64()*(max-min)) * float64(time.Second))
}

// ------------------------------------------------------------------ helpers
//
// The discovery API responses are handled as generic map[string]any (the
// shape json.Unmarshal produces for arbitrary JSON), same as
// internal/convmd's conversation documents. These accessors are the same
// small, never-panicking, zero-value-on-mismatch helpers as
// internal/convmd/render.go's getStr/getMap/getSlice/getFloat; they are
// re-declared here (rather than exported from convmd) because they're each
// a couple of lines and convmd's are deliberately kept unexported.

func getStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func getMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

func getSlice(m map[string]any, key string) []any {
	v, _ := m[key].([]any)
	return v
}

func getFloat(m map[string]any, key string) (float64, bool) {
	f, ok := m[key].(float64)
	return f, ok
}

// idString extracts an id/gid the way Python's truthiness check plus
// f-string interpolation would: `if it.get("id")` keeps any truthy value —
// a non-empty string OR a nonzero number — and `f"{...}/{it['id']}"`
// stringifies it. Real ChatGPT ids are always UUID strings, so the numeric
// branch only guards a hypothetical numeric id: Go's JSON decoder gives
// every JSON number as float64 regardless of whether the source literal had
// a decimal point, so an integral value is rendered the way Python's
// str(int) would (e.g. 123 -> "123") and a non-integral one falls back to
// Go's shortest round-tripping decimal form (close to, though not
// guaranteed byte-identical to, Python's str(float)).
func idString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == 0 {
			return "" // falsy in Python, same as an absent/empty id
		}
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	return ""
}

// cursorTruthy reports whether v is "truthy" in Python's sense for a cursor
// value coming out of JSON (nil, "", and 0 are falsy), matching the sidebar
// pagination's `if cursor:` / `if not cursor:` (export_chats.py:182,198).
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

// -------------------------------------------------------------- discover_main

// DiscoverMain lists every conversation in the account's main sidebar.
// Port of discover_main (export_chats.py:149-174).
func DiscoverMain(f browser.Fetcher, headers map[string]string) ([]Chat, error) {
	var items []map[string]any
	offset := 0
	total := 0
	haveTotal := false

	for {
		// Mirrors `while total is None or offset < total:` checked before
		// each page is fetched: the first page always runs (haveTotal is
		// still false), later pages stop once offset has caught up.
		if haveTotal && offset >= total {
			break
		}

		api := fmt.Sprintf("%s/backend-api/conversations?offset=%d&limit=100&order=updated",
			browser.BaseURL, offset)
		status, body, err := browser.APIGet(f, api, headers)
		if err != nil {
			return nil, err
		}
		if status != 200 {
			return nil, fmt.Errorf(
				"conversation list request failed (HTTP %d). "+
					"You can build a chats.csv by hand instead — see README.", status)
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(body), &data); err != nil {
			return nil, fmt.Errorf("discover: parsing conversation list: %w", err)
		}
		batch := getSlice(data, "items")
		for _, raw := range batch {
			if it, ok := raw.(map[string]any); ok {
				items = append(items, it)
			}
		}
		batchLen := len(batch)
		// total = data.get("total", len(items)) in Python only substitutes
		// len(items) when the "total" key is ABSENT; an explicit
		// `"total": null` yields None, and `while total is None or
		// offset < total` then keeps paging until an empty batch — it must
		// not be treated as "total == len(items) so far", which would stop
		// pagination one page early. getFloat's ok is false both when the
		// key is absent and when it's present-but-null, so both cases are
		// simplified here to "unknown total": haveTotal only becomes true
		// (bounding the loop) when this page actually carried a numeric
		// total; otherwise the empty-batch check below is the sole
		// terminator, which never returns fewer chats than Python would.
		if t, ok := getFloat(data, "total"); ok {
			total = int(t)
			haveTotal = true
		} else {
			haveTotal = false
		}
		offset += 100
		totalStr := "None"
		if haveTotal {
			totalStr = strconv.Itoa(total)
		}
		fsutil.Log(fmt.Sprintf("  found %d/%s", len(items), totalStr))
		if batchLen == 0 {
			break
		}
		sleep(jitter(2, 4))
	}

	chats := make([]Chat, 0, len(items))
	for _, it := range items {
		id := idString(it["id"])
		if id == "" {
			continue
		}
		title := getStr(it, "title")
		if title == "" {
			title = "untitled"
		}
		chats = append(chats, Chat{
			URL:   browser.BaseURL + "/c/" + id,
			ID:    id,
			Title: title,
		})
	}
	return chats, nil
}

// ---------------------------------------------------------- discover_projects

// DiscoverProjects enumerates every Project (gizmo) in the account's
// sidebar and every conversation inside each of them. Port of
// discover_projects (export_chats.py:177-228).
//
// A non-200 response from the sidebar listing is not an error (it logs a
// warning and returns an empty list, matching Python's early `return []`);
// a transport-level error from the Fetcher itself propagates, matching an
// uncaught Python exception from page.evaluate.
func DiscoverProjects(f browser.Fetcher, headers map[string]string) ([]Chat, error) {
	type project struct{ gid, name string }
	var projects []project
	var cursor any // nil == Python's None

	for {
		api := browser.BaseURL + "/backend-api/gizmos/snorlax/sidebar"
		if cursorTruthy(cursor) {
			api += fmt.Sprintf("?cursor=%v", cursor)
		}
		status, body, err := browser.APIGet(f, api, headers)
		if err != nil {
			return nil, err
		}
		if status != 200 {
			fsutil.Log(fmt.Sprintf("  ! project listing failed (HTTP %d), skipping projects", status))
			return nil, nil
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(body), &data); err != nil {
			return nil, fmt.Errorf("discover: parsing project sidebar: %w", err)
		}
		items := getSlice(data, "items")
		for _, raw := range items {
			it, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			// g = it.get("gizmo") or {}; g = g.get("gizmo") or g — the
			// payload nests the real gizmo one level deeper in some shapes.
			g := getMap(it, "gizmo")
			if inner := getMap(g, "gizmo"); len(inner) > 0 {
				g = inner
			}
			gid := idString(g["id"])
			name := getStr(getMap(g, "display"), "name")
			if name == "" {
				name = getStr(g, "short_url")
			}
			if name == "" {
				name = gid
			}
			if name == "" {
				name = "project"
			}
			if gid != "" {
				projects = append(projects, project{gid, name})
			}
		}

		cursor = data["cursor"]
		if !cursorTruthy(cursor) || len(items) == 0 {
			break
		}
		sleep(jitter(1, 2))
	}

	var chats []Chat
	for _, p := range projects {
		got := 0
		// page_cursor starts as the Python int 0; JSON numbers decode to
		// float64 in Go regardless of source formatting, so float64(0) is
		// the natural analogue and compares correctly (by Go's interface
		// equality, which — like Python's `==` across types — only matches
		// when both dynamic type and value agree) against a same-valued
		// numeric cursor from the API, while still comparing unequal to a
		// string cursor such as "0". See package doc / task report for the
		// one case this can't reproduce: Python additionally distinguishes
		// int from float (json.loads("0") -> int, json.loads("0.0") ->
		// float); Go's decoder erases that distinction from the start, so
		// there is no equivalent int-vs-float mismatch to reproduce.
		var pageCursor any = float64(0)
		for {
			api := fmt.Sprintf("%s/backend-api/gizmos/%s/conversations?cursor=%v&limit=50&owned_only=false",
				browser.BaseURL, p.gid, pageCursor)
			status, body, err := browser.APIGet(f, api, headers)
			if err != nil {
				return nil, err
			}
			if status != 200 {
				fsutil.Log(fmt.Sprintf("  ! listing project '%s' failed (HTTP %d)", p.name, status))
				break
			}
			var data map[string]any
			if err := json.Unmarshal([]byte(body), &data); err != nil {
				return nil, fmt.Errorf("discover: parsing project conversations: %w", err)
			}
			batch := getSlice(data, "items")
			for _, raw := range batch {
				it, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				id := idString(it["id"])
				if id == "" {
					continue
				}
				title := getStr(it, "title")
				if title == "" {
					title = "untitled"
				}
				chats = append(chats, Chat{
					URL:   fmt.Sprintf("%s/g/%s/c/%s", browser.BaseURL, p.gid, id),
					ID:    id,
					Title: title,
				})
			}
			got += len(batch)
			nxt := data["cursor"]
			if len(batch) == 0 || nxt == nil || nxt == pageCursor {
				break
			}
			pageCursor = nxt
			sleep(jitter(1, 2))
		}
		fsutil.Log(fmt.Sprintf("  project '%s': %d conversations", p.name, got))
	}
	return chats, nil
}

// ------------------------------------------------------------ discover_chats

// DiscoverChats discovers every conversation (main sidebar + every project)
// and merges them into csvPath, preserving existing rows and their order so
// that export folder numbering stays stable across runs. Port of
// discover_chats (export_chats.py:231-261); see the package doc for why
// headers (rather than a page to capture auth from) is a parameter here.
func DiscoverChats(f browser.Fetcher, headers map[string]string, csvPath string) error {
	fsutil.Log("Discovering conversations in this account...")
	found, err := DiscoverMain(f, headers)
	if err != nil {
		return err
	}
	fsutil.Log("Discovering project conversations...")
	projectChats, err := DiscoverProjects(f, headers)
	if err != nil {
		return err
	}
	found = append(found, projectChats...)

	var existingRows [][2]string
	existingIDs := map[string]bool{}
	if file, err := os.Open(csvPath); err == nil {
		func() {
			defer file.Close()
			r := fsutil.NewBOMTolerantReader(file)
			for {
				row, rerr := r.Read()
				if rerr == io.EOF {
					return
				}
				if rerr != nil {
					err = fmt.Errorf("discover: reading %s: %w", csvPath, rerr)
					return
				}
				if len(row) == 0 {
					continue
				}
				col0 := strings.TrimSpace(row[0])
				if !strings.HasPrefix(col0, "http") {
					continue
				}
				m := ConvIDRE.FindStringSubmatch(col0)
				if m == nil {
					continue
				}
				// row[1].strip() if len(row) > 1 else "untitled" — note this
				// does NOT fall back to "untitled" when row[1] is present
				// but blank, unlike ReadChatList below.
				title := "untitled"
				if len(row) > 1 {
					title = strings.TrimSpace(row[1])
				}
				existingRows = append(existingRows, [2]string{col0, title})
				existingIDs[m[1]] = true
			}
		}()
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("discover: opening %s: %w", csvPath, err)
	}

	var newRows [][2]string
	for _, c := range found {
		if existingIDs[c.ID] {
			continue
		}
		existingIDs[c.ID] = true
		newRows = append(newRows, [2]string{c.URL, c.Title})
	}

	out, err := os.Create(csvPath)
	if err != nil {
		return fmt.Errorf("discover: writing %s: %w", csvPath, err)
	}
	defer out.Close()
	w := csv.NewWriter(out)
	if err := w.Write([]string{"url", "title"}); err != nil {
		return fmt.Errorf("discover: writing %s: %w", csvPath, err)
	}
	for _, row := range existingRows {
		if err := w.Write(row[:]); err != nil {
			return fmt.Errorf("discover: writing %s: %w", csvPath, err)
		}
	}
	for _, row := range newRows {
		if err := w.Write(row[:]); err != nil {
			return fmt.Errorf("discover: writing %s: %w", csvPath, err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("discover: writing %s: %w", csvPath, err)
	}

	fsutil.Log(fmt.Sprintf("%s: kept %d existing, added %d new", csvPath, len(existingRows), len(newRows)))
	return nil
}

// -------------------------------------------------------------- read_chat_list

// ReadChatList returns every conversation in csvPath, deduped by
// conversation id. Any CSV whose first column is a chatgpt.com conversation
// URL is accepted; the second column (optional) is the title. Header rows
// are ignored (they don't look like a URL). Port of read_chat_list
// (export_chats.py:264-286).
func ReadChatList(csvPath string) ([]Chat, error) {
	file, err := os.Open(csvPath)
	if err != nil {
		return nil, fmt.Errorf("discover: opening %s: %w", csvPath, err)
	}
	defer file.Close()

	r := fsutil.NewBOMTolerantReader(file)
	var chats []Chat
	seen := map[string]bool{}
	for {
		row, rerr := r.Read()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("discover: reading %s: %w", csvPath, rerr)
		}
		if len(row) == 0 {
			continue
		}
		url := strings.TrimSpace(row[0])
		if !strings.HasPrefix(url, "http") {
			continue
		}
		title := ""
		if len(row) > 1 {
			title = strings.TrimSpace(row[1])
		}
		if title == "" {
			title = "untitled"
		}
		m := ConvIDRE.FindStringSubmatch(url)
		if m == nil {
			fsutil.Log("  ! no conversation id in URL, skipping: " + url)
			continue
		}
		cid := m[1]
		if seen[cid] {
			continue
		}
		seen[cid] = true
		chats = append(chats, Chat{URL: url, ID: cid, Title: title})
	}
	return chats, nil
}
