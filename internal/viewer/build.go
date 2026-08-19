// Package viewer builds a self-contained HTML viewer (export/viewer.html)
// for exported chats. It is a behavioral port of build_viewer.py; see that
// file for the original implementation and build_test.go for parity
// expectations verified against it.
package viewer

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/saigyo/scrapemychats/internal/fsutil"
)

//go:embed template.html
var templateHTML string

// Category is one (group, sub) entry from categories.json, in the order it
// appeared in the file (after skipping "_"-prefixed keys). Keeping an
// ordered slice — rather than a map — is what lets Classify replicate
// Python dict-iteration order, which determines tie-breaking: Python's
// json.loads preserves key insertion order because dicts (and hence the
// parsed categories mapping) are order-preserving since Python 3.7.
type Category struct {
	Group, Sub string
	Keywords   []string // already lowercased
}

// isPySpace reports whether r is one of the code points Python's
// str.strip() (and str.lstrip()/rstrip() with no argument) treats as
// whitespace. internal/fsutil has a sibling copy of this exact logic
// (unexported there too, for the same reason: it's a byte-for-byte port of
// export_chats.py's own whitespace handling), duplicated here rather than
// exporting it and coupling the two packages together.
func isPySpace(r rune) bool {
	switch r {
	case 0x09, 0x0A, 0x0B, 0x0C, 0x0D,
		0x1C, 0x1D, 0x1E, 0x1F,
		0x20,
		0x85,
		0xA0,
		0x1680,
		0x2028,
		0x2029,
		0x202F,
		0x205F,
		0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// pyStrip trims leading and trailing Python-whitespace runes from s,
// matching Python's str.strip() with no arguments.
func pyStrip(s string) string {
	return strings.TrimFunc(s, isPySpace)
}

// orderedEntry is one key/raw-value pair from a JSON object, in the order
// decodeOrderedObject resolved it to (see that function for what "order"
// means when a key repeats).
type orderedEntry struct {
	key string
	val json.RawMessage
}

// decodeOrderedObject reads one JSON object value from dec and returns its
// top-level key/value pairs, replicating how Python's json.loads actually
// builds a dict: it parses key/value pairs left to right and assigns each
// into the result dict as it goes, so when a key repeats, the LAST
// occurrence's value wins but the key keeps the position of its FIRST
// occurrence (a dict never reorders on reassignment, and a repeated
// object key is never merged with the earlier one — it fully replaces
// it). Using json.RawMessage for values lets the caller decode nested
// objects (categories.json's per-group sub-object) with this same
// last-value-wins-first-position rule, recursively.
func decodeOrderedObject(dec *json.Decoder) ([]orderedEntry, error) {
	if _, err := dec.Token(); err != nil { // '{'
		return nil, err
	}
	var entries []orderedEntry
	index := map[string]int{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("categories JSON: expected object key, got %v", keyTok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if i, ok := index[key]; ok {
			entries[i].val = raw
		} else {
			index[key] = len(entries)
			entries = append(entries, orderedEntry{key: key, val: raw})
		}
	}
	if _, err := dec.Token(); err != nil { // '}'
		return nil, err
	}
	return entries, nil
}

// LoadCategories returns the parsed (group, sub) -> keywords entries in
// file order, the group display order, and the sub display order per
// group, mirroring build_viewer.py's load_categories(). A missing file is
// not an error: it returns empty results, matching Python's
// `if not path.exists(): return {}, [], {}`.
//
// Key ordering matters here (it drives Classify's tie-breaking and the
// viewer's category rail), so this walks the JSON token stream by hand
// (via decodeOrderedObject) instead of unmarshaling into a map, which in
// Go has randomized iteration order — and, unlike a plain map, also
// reproduces Python's last-value-wins-at-first-position behavior for a
// categories.json with a duplicate group or sub key (an edit mistake, but
// one build_viewer.py silently tolerates, so this does too).
func LoadCategories(path string) ([]Category, []string, map[string][]string, error) {
	groupOrder := []string{}
	subOrder := map[string][]string{}
	var categories []Category

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return categories, groupOrder, subOrder, nil
		}
		return nil, nil, nil, err
	}

	topEntries, err := decodeOrderedObject(json.NewDecoder(bytes.NewReader(data)))
	if err != nil {
		return nil, nil, nil, err
	}

	for _, ge := range topEntries {
		group := ge.key
		if strings.HasPrefix(group, "_") {
			continue
		}
		groupOrder = append(groupOrder, group)

		subEntries, err := decodeOrderedObject(json.NewDecoder(bytes.NewReader(ge.val)))
		if err != nil {
			return nil, nil, nil, err
		}

		subs := []string{}
		for _, se := range subEntries {
			sub := se.key
			if strings.HasPrefix(sub, "_") {
				continue
			}
			subs = append(subs, sub)
			var words []string
			if err := json.Unmarshal(se.val, &words); err != nil {
				return nil, nil, nil, err
			}
			lowered := make([]string, len(words))
			for i, w := range words {
				lowered[i] = pyLower(w)
			}
			categories = append(categories, Category{Group: group, Sub: sub, Keywords: lowered})
		}
		subOrder[group] = subs
	}

	return categories, groupOrder, subOrder, nil
}

// pyLower approximates Python's str.lower(), which build_viewer.py's
// classify() (via title.lower() / body.lower()) and load_categories() (via
// w.lower()) both rely on for case-insensitive keyword matching. Go's
// unicode.ToLower is a 1:1 rune-to-rune mapping and gets the vast majority
// of Unicode right, but diverges from Python's str.lower() — which uses
// Unicode's full case-folding tables, not simple case mapping — in two
// documented ways:
//
//  1. U+0130 LATIN CAPITAL LETTER I WITH DOT ABOVE (İ) lowercases in
//     Python to the TWO code points U+0069 U+0307 ("i" + COMBINING DOT
//     ABOVE), not a single rune. unicode.ToLower, being 1:1, maps it to
//     plain "i" and silently drops the combining dot.
//  2. U+03A3 GREEK CAPITAL LETTER SIGMA (Σ) lowercases context-sensitively
//     in Python via the Final_Sigma rule: to U+03C2 (final sigma, ς) when
//     it ends a "word" — preceded by a cased letter and not followed by
//     one — and to U+03C3 (σ) otherwise. unicode.ToLower always maps it
//     to U+03C3.
//
// The Final_Sigma check below approximates Unicode's real definition
// (which involves skipping case-ignorable code points and a proper
// word-break scan) by using unicode.IsLetter as a stand-in for "cased
// letter" on the immediately adjacent rune. Known remaining divergences
// from str.lower() are exactly the cases where that approximation shows:
// a Σ adjacent to an uncased letter (CJK, Hebrew — IsLetter says true,
// Python says not cased) or to a case-ignorable code point (apostrophe,
// combining marks — Python skips these when scanning, this code does
// not). These only matter when a category keyword itself contains a
// sigma in such a context. Beyond that, differences can also stem from
// Unicode-Character-Database version skew between Go's and Python's
// standard libraries.
func pyLower(s string) string {
	runes := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range runes {
		switch r {
		case 0x0130: // İ
			b.WriteRune('i')
			b.WriteRune(0x0307)
		case 0x03A3: // Σ
			precededByCased := i > 0 && unicode.IsLetter(runes[i-1])
			followedByCased := i+1 < len(runes) && unicode.IsLetter(runes[i+1])
			if precededByCased && !followedByCased {
				b.WriteRune(0x03C2) // ς final sigma
			} else {
				b.WriteRune(0x03C3) // σ
			}
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// Classify scores title/body against every category and returns the
// (group, sub) of the best match, mirroring build_viewer.py's classify().
// Title hits count 4x (substring match against " "+lower(title)+" "), body
// hits count 1x (substring match against the first 4000 runes of the body,
// lowercased AFTER slicing — Python slices body[:4000] first, then calls
// .lower() on the slice; this replicates that order, though for
// case-folding-sensitive scripts the order could in principle matter).
// Ties keep whichever category reached the best score first in file
// (insertion) order, since only a strictly greater score replaces the
// current best. No category ever scoring above 0 yields ("Unsorted",
// "Unsorted"); an empty categories list yields ("", "").
func Classify(title, body string, categories []Category) (string, string) {
	if len(categories) == 0 {
		return "", ""
	}
	t := " " + pyLower(title) + " "

	runes := []rune(body)
	n := len(runes)
	if n > 4000 {
		n = 4000
	}
	b := " " + pyLower(string(runes[:n])) + " "

	var bestGroup, bestSub string
	bestScore := 0
	found := false
	for _, cat := range categories {
		score := 0
		for _, w := range cat.Keywords {
			if strings.Contains(t, w) {
				score += 4
			} else if strings.Contains(b, w) {
				score++
			}
		}
		if score > bestScore {
			bestGroup, bestSub = cat.Group, cat.Sub
			bestScore = score
			found = true
		}
	}
	if !found {
		return "Unsorted", "Unsorted"
	}
	return bestGroup, bestSub
}

// Chat is one exported conversation, ready to be embedded as one element of
// the __DATA__ JSON array. Field order matches build_viewer.py's parse_chat
// dict literal (id, t, d, u, g, s, f, n, b), which Go struct field order
// reproduces in json.Marshal output.
type Chat struct {
	ID string   `json:"id"`
	T  string   `json:"t"`
	D  string   `json:"d"`
	U  string   `json:"u"`
	G  string   `json:"g"`
	S  string   `json:"s"`
	F  []string `json:"f"`
	N  int      `json:"n"`
	B  string   `json:"b"`
}

var messageHeaderRe = regexp.MustCompile(`(?m)^## `)

// ParseChat reads folder/conversation.md and builds its Chat record,
// mirroring build_viewer.py's parse_chat(). A missing conversation.md is
// not an error: it returns (nil, nil), matching Python's
// `if not md_path.exists(): return None`.
func ParseChat(folder string, categories []Category) (*Chat, error) {
	mdPath := filepath.Join(folder, "conversation.md")
	data, err := os.ReadFile(mdPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	folderName := filepath.Base(folder)

	// Python's Path.read_text() opens in universal-newline mode, which
	// translates "\r\n" and a lone "\r" to "\n" before any splitting
	// happens. export_chats.py can produce CRLF line endings when run on
	// Windows, so without this normalization a Windows-exported
	// conversation.md would parse into one giant line here instead of the
	// expected line-per-line structure.
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")

	title := folderName
	if len(lines) > 0 {
		title = pyStrip(strings.TrimLeft(lines[0], "# "))
	}

	var url, date string
	bodyStart := 0
	foundHeader := false
	limit := 8
	if limit > len(lines) {
		limit = len(lines)
	}
	for i := 0; i < limit; i++ {
		ln := lines[i]
		switch {
		case strings.HasPrefix(ln, "- URL: "):
			url = pyStrip(ln[len("- URL: "):])
		case strings.HasPrefix(ln, "- create time: "):
			date = pyStrip(ln[len("- create time: "):])
		case strings.HasPrefix(ln, "- update time: ") && date == "":
			date = pyStrip(ln[len("- update time: "):])
		}
		if strings.HasPrefix(ln, "## ") {
			bodyStart = i
			foundHeader = true
			break
		}
	}
	if !foundHeader {
		bodyStart = 6
		if len(lines) < 6 {
			bodyStart = len(lines)
		}
	}
	body := pyStrip(strings.Join(lines[bodyStart:], "\n"))

	files := []string{}
	fdir := filepath.Join(folder, "files")
	if info, err := os.Stat(fdir); err == nil && info.IsDir() {
		// os.ReadDir already returns entries sorted by filename (byte-wise,
		// matching Python's sorted() on same-directory Path objects), so no
		// separate sort is needed here.
		entries, err := os.ReadDir(fdir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			files = append(files, folderName+"/files/"+e.Name())
		}
	}

	group, sub := Classify(title, body, categories)

	return &Chat{
		ID: folderName,
		T:  title,
		D:  date,
		U:  url,
		G:  group,
		S:  sub,
		F:  files,
		N:  len(messageHeaderRe.FindAllStringIndex(body, -1)),
		B:  body,
	}, nil
}

// Meta is the __META__ JSON object. Field order matches build_viewer.py's
// meta dict literal (count, from, to, title, groups, subs).
//
// Subs is a map[string][]string (group -> ordered sub list), so
// json.Marshal emits its keys alphabetically rather than in Python's
// group-insertion order. This does not affect the viewer: the template's
// JS (see buildRail() in template.html) iterates META.groups — the
// ordered array — and looks sub lists up by key (`META.subs[grp]`), so the
// object's own key order in the JSON text is never observed.
type Meta struct {
	Count  int                 `json:"count"`
	From   string              `json:"from"`
	To     string              `json:"to"`
	Title  string              `json:"title"`
	Groups []string            `json:"groups"`
	Subs   map[string][]string `json:"subs"`
}

// runeSlice returns the first n runes of s (or all of s if it has fewer),
// matching Python's s[:n] which slices by code point, not byte.
func runeSlice(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// encodeJSON renders v the way build_viewer.py's json.dumps(v,
// ensure_ascii=False).replace("</", "<\\/") does: HTML-unescaped UTF-8,
// with any "</" sequence escaped so a literal "</script>" inside chat text
// cannot break out of the embedding <script type="application/json">
// block. It intentionally does NOT try to reproduce Python's json.dumps
// whitespace (", " / ": " separators) — Go's encoder emits compact
// "," / ":" separators instead. The two are not byte-identical, but the
// viewer's JS only ever JSON.parses this text, so the parsed value is all
// that matters; see build_test.go for the semantic-equality check against
// a Python-generated golden.
//
// Go's encoding/json, even with SetEscapeHTML(false), still backslash-
// escapes U+2028/U+2029 (LINE/PARAGRAPH SEPARATOR) inside string values —
// unlike Python's json.dumps(ensure_ascii=False), which emits them as raw
// UTF-8. This is a real (if narrow) divergence from Python's byte output,
// but both forms decode to the identical string under JSON.parse, so it
// does not affect the viewer. Verified empirically in
// TestEncodeJSON_LineSeparatorEscaping.
func encodeJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	s := strings.TrimSuffix(buf.String(), "\n")
	s = strings.ReplaceAll(s, "</", `<\/`)
	return s, nil
}

// Build scans exportDir for exported chats, classifies them against
// categoriesPath (if present), and writes exportDir/viewer.html — a
// self-contained HTML archive viewer. It mirrors build_viewer.py's main().
func Build(exportDir, categoriesPath, title string) error {
	categories, groupOrder, subOrder, err := LoadCategories(categoriesPath)
	if err != nil {
		return err
	}
	if len(categories) > 0 {
		fsutil.Log(fmt.Sprintf("categories loaded from %s", categoriesPath))
	} else {
		fsutil.Log("no categories.json — building a flat archive (copy categories.example.json to categories.json to enable)")
	}

	// os.ReadDir already returns entries sorted by filename (byte-wise,
	// matching Python's sorted() on Path objects), so no separate sort is
	// needed here. os.Stat (rather than DirEntry.IsDir()) is used below so
	// a symlink to a directory is followed, matching Python's
	// Path.is_dir().
	entries, err := os.ReadDir(exportDir)
	if err != nil {
		return err
	}

	var chats []*Chat
	for _, e := range entries {
		full := filepath.Join(exportDir, e.Name())
		info, err := os.Stat(full)
		if err != nil || !info.IsDir() {
			continue
		}
		c, err := ParseChat(full, categories)
		if err != nil {
			return err
		}
		if c != nil {
			chats = append(chats, c)
		}
	}
	if len(chats) == 0 {
		return fmt.Errorf("no exported chats found in %s/ — run export_chats.py first", exportDir)
	}

	sort.SliceStable(chats, func(i, j int) bool {
		ki, kj := chats[i].D, chats[j].D
		if ki == "" {
			ki = "0000"
		}
		if kj == "" {
			kj = "0000"
		}
		return ki > kj
	})

	presentUnsorted := false
	haveUnsortedGroup := false
	for _, c := range chats {
		if c.G == "Unsorted" && c.S == "Unsorted" {
			presentUnsorted = true
		}
	}
	for _, g := range groupOrder {
		if g == "Unsorted" {
			haveUnsortedGroup = true
		}
	}
	if presentUnsorted && !haveUnsortedGroup {
		groupOrder = append(groupOrder, "Unsorted")
		subOrder["Unsorted"] = []string{"Unsorted"}
	}

	var dates []string
	for _, c := range chats {
		if c.D != "" {
			dates = append(dates, runeSlice(c.D, 10))
		}
	}
	from, to := "", ""
	if len(dates) > 0 {
		from, to = dates[0], dates[0]
		for _, d := range dates[1:] {
			if d < from {
				from = d
			}
			if d > to {
				to = d
			}
		}
	}

	meta := Meta{
		Count:  len(chats),
		From:   from,
		To:     to,
		Title:  title,
		Groups: groupOrder,
		Subs:   subOrder,
	}

	if len(categories) > 0 {
		type gs struct{ g, s string }
		dist := map[gs]int{}
		for _, c := range chats {
			dist[gs{c.G, c.S}]++
		}
		fsutil.Log(fmt.Sprintf("%d chats classified:", len(chats)))
		for _, g := range groupOrder {
			fsutil.Log(fmt.Sprintf("  %s", g))
			for _, s := range subOrder[g] {
				if n, ok := dist[gs{g, s}]; ok {
					fsutil.Log(fmt.Sprintf("    %-24s %d", s, n))
				}
			}
		}
	}

	dataJSON, err := encodeJSON(chats)
	if err != nil {
		return err
	}
	metaJSON, err := encodeJSON(meta)
	if err != nil {
		return err
	}

	html := strings.ReplaceAll(templateHTML, "__META__", metaJSON)
	html = strings.ReplaceAll(html, "__DATA__", dataJSON)

	out := filepath.Join(exportDir, "viewer.html")
	htmlBytes := []byte(html)
	if err := os.WriteFile(out, htmlBytes, 0o644); err != nil {
		return err
	}
	fsutil.Log(fmt.Sprintf("wrote %s (%.1f MB) — open it in a browser", out, float64(len(htmlBytes))/1e6))

	return nil
}
