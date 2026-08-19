package viewer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// copyDir recursively copies src into dst, creating dst if needed.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("copyDir ReadDir(%s): %v", src, err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("copyDir MkdirAll(%s): %v", dst, err)
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyDir(t, s, d)
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			t.Fatalf("copyDir ReadFile(%s): %v", s, err)
		}
		if err := os.WriteFile(d, data, 0o644); err != nil {
			t.Fatalf("copyDir WriteFile(%s): %v", d, err)
		}
	}
}

// scriptBlockRe extracts the two <script id="meta"|"data" ...>...</script>
// JSON payload blocks so tests can isolate the byte-critical HTML shell
// from the JSON blobs (whose whitespace is allowed to differ from
// Python's — see encodeJSON's doc comment).
var scriptBlockRe = regexp.MustCompile(`(?s)(<script id="(?:meta|data)" type="application/json">)(.*?)(</script>)`)

// shellAndBlobs splits html into "the shell with both JSON payloads
// replaced by a placeholder" and the two payload strings in document
// order (meta, then data).
func shellAndBlobs(t *testing.T, html string) (shell string, blobs []string) {
	t.Helper()
	matches := scriptBlockRe.FindAllStringSubmatchIndex(html, -1)
	if len(matches) != 2 {
		t.Fatalf("expected 2 script JSON blocks, found %d", len(matches))
	}
	var sb strings.Builder
	last := 0
	for _, m := range matches {
		// m[0],m[1] = whole match; m[4],m[5] = group 2 (the payload)
		sb.WriteString(html[last:m[4]])
		sb.WriteString("\x00PAYLOAD\x00")
		blobs = append(blobs, html[m[4]:m[5]])
		last = m[5]
	}
	sb.WriteString(html[last:])
	return sb.String(), blobs
}

func TestBuild_GoldenParity(t *testing.T) {
	tmp := t.TempDir()
	fixture := filepath.Join("testdata", "fixture")
	copyDir(t, filepath.Join(fixture, "export"), filepath.Join(tmp, "export"))

	categoriesPath := filepath.Join(tmp, "categories.json")
	catData, err := os.ReadFile(filepath.Join(fixture, "categories.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(categoriesPath, catData, 0o644); err != nil {
		t.Fatal(err)
	}

	exportDir := filepath.Join(tmp, "export")
	if err := Build(exportDir, categoriesPath, "Test Archive"); err != nil {
		t.Fatalf("Build: %v", err)
	}

	gotHTML, err := os.ReadFile(filepath.Join(exportDir, "viewer.html"))
	if err != nil {
		t.Fatalf("reading generated viewer.html: %v", err)
	}
	wantHTML, err := os.ReadFile(filepath.Join("testdata", "golden", "viewer.html"))
	if err != nil {
		t.Fatalf("reading golden viewer.html: %v", err)
	}

	gotShell, gotBlobs := shellAndBlobs(t, string(gotHTML))
	wantShell, wantBlobs := shellAndBlobs(t, string(wantHTML))

	// (a) The HTML shell — everything outside the two JSON payloads — must
	// be byte-identical to what build_viewer.py produces on the same
	// fixture.
	if gotShell != wantShell {
		t.Errorf("HTML shell differs from Python golden (byte-for-byte comparison)")
		// Show the first differing byte offset to aid debugging.
		n := len(gotShell)
		if len(wantShell) < n {
			n = len(wantShell)
		}
		for i := 0; i < n; i++ {
			if gotShell[i] != wantShell[i] {
				lo, hi := i-40, i+40
				if lo < 0 {
					lo = 0
				}
				if hi > n {
					hi = n
				}
				t.Errorf("first diff at byte %d\n got: %q\nwant: %q", i, gotShell[lo:hi], wantShell[lo:hi])
				break
			}
		}
		if len(gotShell) != len(wantShell) {
			t.Errorf("shell length differs: got %d want %d", len(gotShell), len(wantShell))
		}
	}

	if len(gotBlobs) != 2 || len(wantBlobs) != 2 {
		t.Fatalf("expected 2 blobs each, got %d/%d", len(gotBlobs), len(wantBlobs))
	}

	// (c) "</" must never appear unescaped inside either JSON blob — it is
	// always rewritten to "<\/" so a literal "</script>" in chat text
	// cannot terminate the embedding <script> element early.
	for i, blob := range gotBlobs {
		if strings.Contains(blob, "</") {
			t.Errorf("blob %d contains an unescaped \"</\" sequence", i)
		}
	}

	// (b) The parsed __META__/__DATA__ JSON must be semantically equal to
	// Python's, even though the raw whitespace differs (Go's encoder emits
	// compact separators; Python's json.dumps uses ", "/": "). Parse both
	// into generic structures and compare with reflect.DeepEqual via
	// encoding/json's own equality (round-trip through an ordered-agnostic
	// map/slice comparison).
	assertJSONEqual(t, "meta", gotBlobs[0], mustReadFile(t, filepath.Join("testdata", "golden", "meta.json")))
	assertJSONEqual(t, "data", gotBlobs[1], mustReadFile(t, filepath.Join("testdata", "golden", "data.json")))
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func assertJSONEqual(t *testing.T, label, gotJSON, wantJSON string) {
	t.Helper()
	var got, want any
	if err := json.Unmarshal([]byte(gotJSON), &got); err != nil {
		t.Fatalf("%s: unmarshaling Go output: %v", label, err)
	}
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("%s: unmarshaling Python golden: %v", label, err)
	}
	gotCanon, _ := json.Marshal(got)
	wantCanon, _ := json.Marshal(want)
	if string(gotCanon) != string(wantCanon) {
		t.Errorf("%s: parsed JSON differs from Python golden\n got: %s\nwant: %s", label, gotCanon, wantCanon)
	}
}

func TestBuild_NoChatsFound(t *testing.T) {
	tmp := t.TempDir()
	exportDir := filepath.Join(tmp, "export")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	err := Build(exportDir, filepath.Join(tmp, "categories.json"), "Empty Archive")
	if err == nil {
		t.Fatal("expected error for empty export dir, got nil")
	}
	want := "no exported chats found in " + exportDir + "/ — run export_chats.py first"
	if err.Error() != want {
		t.Errorf("error message = %q, want %q", err.Error(), want)
	}
}

func TestLoadCategories_OrderAndUnderscoreSkip(t *testing.T) {
	categories, groupOrder, subOrder, err := LoadCategories(filepath.Join("testdata", "fixture", "categories.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Work", "Personal"}; !equalStrings(groupOrder, want) {
		t.Errorf("groupOrder = %v, want %v", groupOrder, want)
	}
	if want := []string{"Code & Data"}; !equalStrings(subOrder["Work"], want) {
		t.Errorf("subOrder[Work] = %v, want %v", subOrder["Work"], want)
	}
	if want := []string{"Food & Cooking"}; !equalStrings(subOrder["Personal"], want) {
		t.Errorf("subOrder[Personal] = %v, want %v", subOrder["Personal"], want)
	}
	if len(categories) != 2 {
		t.Fatalf("len(categories) = %d, want 2", len(categories))
	}
	if categories[0].Group != "Work" || categories[0].Sub != "Code & Data" {
		t.Errorf("categories[0] = %+v", categories[0])
	}
	if categories[1].Group != "Personal" || categories[1].Sub != "Food & Cooking" {
		t.Errorf("categories[1] = %+v", categories[1])
	}
	// "_comment" keys, at both the top level and within a group, must be
	// skipped entirely — not turned into a Category with empty keywords.
	for _, c := range categories {
		if strings.HasPrefix(c.Group, "_") || strings.HasPrefix(c.Sub, "_") {
			t.Errorf("category with underscore-prefixed key leaked through: %+v", c)
		}
	}
}

func TestLoadCategories_MissingFile(t *testing.T) {
	categories, groupOrder, subOrder, err := LoadCategories(filepath.Join("testdata", "does-not-exist.json"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if len(categories) != 0 || len(groupOrder) != 0 || len(subOrder) != 0 {
		t.Errorf("expected all-empty results for missing file, got categories=%v groupOrder=%v subOrder=%v", categories, groupOrder, subOrder)
	}
}

func TestLoadCategories_DuplicateKeyLastValueWinsAtFirstPosition(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "categories.json")
	// Python's json.loads builds its dict by repeated key assignment as it
	// parses, so a duplicate top-level "Work" key keeps its FIRST
	// position in iteration order but ends up holding the LAST
	// occurrence's value, entirely replacing (not merging with) the
	// first. Same rule applies to a repeated sub key within one group.
	raw := `{"Work": {"A": ["alpha"]}, "Personal": {"B": ["beta"]}, "Work": {"C": ["zeta"]}}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	categories, groupOrder, subOrder, err := LoadCategories(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Work", "Personal"}; !equalStrings(groupOrder, want) {
		t.Errorf("groupOrder = %v, want %v (Work keeps its first position)", groupOrder, want)
	}
	if want := []string{"C"}; !equalStrings(subOrder["Work"], want) {
		t.Errorf("subOrder[Work] = %v, want %v (the earlier \"A\" sub must be fully replaced, not merged)", subOrder["Work"], want)
	}
	if want := []string{"B"}; !equalStrings(subOrder["Personal"], want) {
		t.Errorf("subOrder[Personal] = %v, want %v", subOrder["Personal"], want)
	}
	if len(categories) != 2 {
		t.Fatalf("len(categories) = %d, want 2 (the discarded \"A\" sub must not survive)", len(categories))
	}

	// A chat scoring against both keywords must land in the surviving
	// (Work, C) category, exactly matching build_viewer.py's output for
	// this same categories.json.
	g, s := Classify("alpha zeta", "", categories)
	if g != "Work" || s != "C" {
		t.Errorf("Classify(%q) = (%q, %q), want (Work, C)", "alpha zeta", g, s)
	}
}

func TestClassify_NoCategories(t *testing.T) {
	g, s := Classify("anything", "anything", nil)
	if g != "" || s != "" {
		t.Errorf("Classify with no categories = (%q, %q), want (\"\", \"\")", g, s)
	}
}

func TestClassify_NoMatch(t *testing.T) {
	cats := []Category{{Group: "Work", Sub: "Code", Keywords: []string{"python"}}}
	g, s := Classify("gardening tips", "how to grow tomatoes", cats)
	if g != "Unsorted" || s != "Unsorted" {
		t.Errorf("Classify with no keyword hits = (%q, %q), want (Unsorted, Unsorted)", g, s)
	}
}

func TestClassify_TitleOutweighsBody(t *testing.T) {
	// "python" in the title scores 4; three body-only keyword hits in the
	// other category would only score 3, so the title match must win.
	cats := []Category{
		{Group: "A", Sub: "TitleHit", Keywords: []string{"python"}},
		{Group: "B", Sub: "BodyHits", Keywords: []string{"one", "two", "three"}},
	}
	g, s := Classify("python question", "one two three", cats)
	if g != "A" || s != "TitleHit" {
		t.Errorf("Classify = (%q, %q), want (A, TitleHit)", g, s)
	}
}

func TestClassify_TieKeepsFirstInOrder(t *testing.T) {
	// Both categories score 4 (one title hit each); Python's `score >
	// best_score` (strict) means the FIRST category to reach that score
	// keeps it — a later equal score never displaces it.
	cats := []Category{
		{Group: "First", Sub: "Sub1", Keywords: []string{"alpha"}},
		{Group: "Second", Sub: "Sub2", Keywords: []string{"beta"}},
	}
	g, s := Classify("alpha beta", "", cats)
	if g != "First" || s != "Sub1" {
		t.Errorf("Classify tie = (%q, %q), want (First, Sub1)", g, s)
	}
}

func TestClassify_BodySliceThenLower(t *testing.T) {
	// Body matching only looks at the first 4000 runes; a keyword that
	// appears only after that point must not match.
	long := strings.Repeat("x", 4000) + "PYTHON"
	cats := []Category{{Group: "G", Sub: "S", Keywords: []string{"python"}}}
	g, s := Classify("", long, cats)
	if g != "Unsorted" || s != "Unsorted" {
		t.Errorf("keyword past the 4000-rune cutoff should not match; got (%q, %q)", g, s)
	}

	short := strings.Repeat("x", 3990) + "PYTHON"
	g, s = Classify("", short, cats)
	if g != "G" || s != "S" {
		t.Errorf("keyword within the 4000-rune cutoff should match (case-insensitively); got (%q, %q)", g, s)
	}
}

func TestClassify_TurkishDotlessCapitalI(t *testing.T) {
	// Python's str.lower() maps U+0130 (İ, LATIN CAPITAL LETTER I WITH DOT
	// ABOVE) to TWO code points: U+0069 "i" + U+0307 COMBINING DOT ABOVE.
	// The keyword below is written the way a categories.json entry would
	// end up after LoadCategories lowers it with pyLower, so this
	// exercises Classify's own title-lowering path against that same
	// two-codepoint sequence. Verified against CPython:
	// "İstanbul trip".lower() == "i̇stanbul trip".
	keyword := "i̇stanbul"
	cats := []Category{{Group: "G", Sub: "S", Keywords: []string{keyword}}}
	g, s := Classify("İstanbul trip", "", cats)
	if g != "G" || s != "S" {
		t.Errorf("Classify with dotless-I keyword = (%q, %q), want (G, S)", g, s)
	}
}

func TestClassify_GreekFinalSigma(t *testing.T) {
	// Python's str.lower() applies the Final_Sigma rule: a word-final
	// capital Sigma (Σ, U+03A3) lowercases to ς (final sigma, U+03C2), not
	// the usual σ (U+03C3). Verified against CPython:
	// "ΣΟΦΟΣ".lower() ==
	// "σοφος" — i.e. "ΣΟΦΟΣ".lower() == "σοφος"
	// (note the trailing ς, not σ).
	word := "ΣΟΦΟΣ"    // ΣΟΦΟΣ
	keyword := "σοφος" // σοφος
	cats := []Category{{Group: "G", Sub: "S", Keywords: []string{keyword}}}
	g, s := Classify(word, "", cats)
	if g != "G" || s != "S" {
		t.Errorf("Classify with final-sigma keyword = (%q, %q), want (G, S)", g, s)
	}
}

func TestParseChat_TitleAndFields(t *testing.T) {
	c, err := ParseChat(filepath.Join("testdata", "fixture", "export", "chat-python-help"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("expected a Chat, got nil")
	}
	if c.T != "Python list comprehension help" {
		t.Errorf("T = %q", c.T)
	}
	if c.U != "https://chat.openai.com/c/aaa111" {
		t.Errorf("U = %q", c.U)
	}
	if c.D != "2024-03-15T10:22:00Z" {
		t.Errorf("D = %q, want create time (not update time)", c.D)
	}
	if len(c.F) != 2 || c.F[0] != "chat-python-help/files/notes.txt" || c.F[1] != "chat-python-help/files/screenshot.png" {
		t.Errorf("F = %v", c.F)
	}
	if c.N != 2 {
		t.Errorf("N = %d, want 2", c.N)
	}
	if !strings.HasPrefix(c.B, "## User") {
		t.Errorf("B should start at the first '## ' header line, not the title/metadata lines above it: %q", c.B[:min(20, len(c.B))])
	}
}

func TestParseChat_UpdateTimeFallback(t *testing.T) {
	// chat-unicode-danger has only "- update time:", no "- create time:",
	// so date must fall back to the update time.
	c, err := ParseChat(filepath.Join("testdata", "fixture", "export", "chat-unicode-danger"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("expected a Chat, got nil")
	}
	if c.D != "2024-01-05T08:00:00Z" {
		t.Errorf("D = %q, want update-time fallback", c.D)
	}
	if c.T != "Emoji café résumé 日本語 test" {
		t.Errorf("T = %q", c.T)
	}
}

func TestParseChat_NoDate(t *testing.T) {
	c, err := ParseChat(filepath.Join("testdata", "fixture", "export", "chat-nodate"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.D != "" {
		t.Errorf("D = %q, want empty", c.D)
	}
	if len(c.F) != 0 {
		t.Errorf("F = %v, want empty (no files/ dir)", c.F)
	}
}

func TestParseChat_MissingConversationMD(t *testing.T) {
	c, err := ParseChat(filepath.Join("testdata", "fixture", "export", "chat-no-md"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		t.Errorf("expected nil for a folder without conversation.md, got %+v", c)
	}
}

func TestParseChat_CRLFNormalization(t *testing.T) {
	// Python's Path.read_text() opens in universal-newline mode, translating
	// "\r\n" to "\n" before any splitting. export_chats.py can write CRLF
	// line endings when run on Windows, so a CRLF conversation.md must
	// parse identically to the same content with LF-only line endings.
	tmp := t.TempDir()
	lfContent := "# CRLF test chat\n\n- URL: https://example.com/c/xyz\n- create time: 2024-05-01T00:00:00Z\n\n## User\n\nhello there\n\n## Assistant\n\nhi back\n"
	crlfContent := strings.ReplaceAll(lfContent, "\n", "\r\n")

	crlfFolder := filepath.Join(tmp, "chat-crlf")
	if err := os.MkdirAll(crlfFolder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crlfFolder, "conversation.md"), []byte(crlfContent), 0o644); err != nil {
		t.Fatal(err)
	}

	lfFolder := filepath.Join(tmp, "chat-lf")
	if err := os.MkdirAll(lfFolder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lfFolder, "conversation.md"), []byte(lfContent), 0o644); err != nil {
		t.Fatal(err)
	}

	crlf, err := ParseChat(crlfFolder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if crlf == nil {
		t.Fatal("expected a Chat, got nil")
	}
	want, err := ParseChat(lfFolder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want == nil {
		t.Fatal("expected a Chat, got nil")
	}

	if crlf.T != want.T || crlf.D != want.D || crlf.U != want.U || crlf.N != want.N || crlf.B != want.B {
		t.Errorf("CRLF parse diverged from LF parse:\n CRLF: %+v\n   LF: %+v", crlf, want)
	}
	if crlf.T != "CRLF test chat" {
		t.Errorf("T = %q", crlf.T)
	}
	if crlf.N != 2 {
		t.Errorf("N = %d, want 2", crlf.N)
	}
	if strings.Contains(crlf.B, "\r") {
		t.Errorf("B still contains a carriage return: %q", crlf.B)
	}
}

func TestParseChat_LoneCRNormalization(t *testing.T) {
	// Old-Mac-style lone "\r" line endings (no "\n" anywhere) must also be
	// normalized, matching Python's universal-newline read.
	tmp := t.TempDir()
	folder := filepath.Join(tmp, "chat-lonecr")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# Lone CR test\r\r- URL: https://example.com/c/abc\r\r## User\r\rhi\r"
	if err := os.WriteFile(filepath.Join(folder, "conversation.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := ParseChat(folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("expected a Chat, got nil")
	}
	if c.T != "Lone CR test" {
		t.Errorf("T = %q", c.T)
	}
	if c.U != "https://example.com/c/abc" {
		t.Errorf("U = %q", c.U)
	}
	if strings.Contains(c.B, "\r") {
		t.Errorf("B still contains a carriage return: %q", c.B)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestEncodeJSON_LineSeparatorEscaping documents a real, narrow divergence
// from Python's json.dumps(ensure_ascii=False): Go's encoding/json escapes
// U+2028 (LINE SEPARATOR) and U+2029 (PARAGRAPH SEPARATOR) as \u2028/\u2029
// even with SetEscapeHTML(false), whereas Python emits them as raw UTF-8
// bytes (verified empirically: json.dumps({"x": "a\u2028b\u2029c"},
// ensure_ascii=False).encode("utf-8") produces the raw 3-byte UTF-8
// sequences for each character, not backslash escapes). Both forms are
// valid JSON and decode to the identical string under JSON.parse, so the
// viewer is unaffected -- but the raw bytes are not identical to Python's
// output, which is why the golden-parity test above only requires
// HTML-shell byte-identity and JSON *semantic* equality, not
// byte-identity of the JSON blobs.
func TestEncodeJSON_LineSeparatorEscaping(t *testing.T) {
	s := "before\u2028after\u2029end"
	encoded, err := encodeJSON(map[string]string{"x": s})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded, `\u2028`) || !strings.Contains(encoded, `\u2029`) {
		t.Fatalf("expected Go's encoder to backslash-escape U+2028/U+2029, got %q", encoded)
	}
	if strings.ContainsRune(encoded, '\u2028') || strings.ContainsRune(encoded, '\u2029') {
		t.Fatalf("did not expect raw U+2028/U+2029 bytes in Go's output, got %q", encoded)
	}
	// Round-trips correctly regardless of representation.
	var decoded map[string]string
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["x"] != s {
		t.Errorf("round-trip mismatch: got %q want %q", decoded["x"], s)
	}
}
