package files

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureRoot is shared with internal/convmd: the same conversation.json
// fixtures back the Markdown goldens and the reference goldens, and the
// Python generator wrote all of them in one pass.
const fixtureRoot = "../convmd/testdata"

// goldenFileRef and goldenSandboxRef mirror the JSON the generator wrote
// from Python's collect_file_refs() / collect_sandbox_refs() results.
type goldenFileRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type goldenSandboxRef struct {
	MessageID string `json:"message_id"`
	Path      string `json:"path"`
}

// fixtures lists the fixture directory names.
func fixtures(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(fixtureRoot)
	if err != nil {
		t.Fatalf("read %s: %v", fixtureRoot, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatalf("no fixtures found in %s", fixtureRoot)
	}
	return names
}

// loadFixture returns the fixture's raw bytes and its decoded form.
func loadFixture(t *testing.T, name string) ([]byte, map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureRoot, name, "conversation.json"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var conv map[string]any
	if err := json.Unmarshal(raw, &conv); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return raw, conv
}

// loadGolden decodes one of the fixture's golden ref files into v.
func loadGolden(t *testing.T, name, file string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureRoot, name, file))
	if err != nil {
		t.Fatalf("read golden %s/%s: %v", name, file, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode golden %s/%s: %v", name, file, err)
	}
}

func TestCollectFileRefs_GoldenParity(t *testing.T) {
	for _, name := range fixtures(t) {
		t.Run(name, func(t *testing.T) {
			raw, conv := loadFixture(t, name)
			var want []goldenFileRef
			loadGolden(t, name, "file_refs.json", &want)

			got := CollectFileRefs(conv, MappingOrder(raw))
			if len(got) != len(want) {
				t.Fatalf("CollectFileRefs = %+v, want %+v", got, want)
			}
			for i := range want {
				// Order matters: it is the order the downloader walks.
				if got[i].ID != want[i].ID || got[i].Name != want[i].Name {
					t.Errorf("ref %d = {%s %s}, want {%s %s}",
						i, got[i].ID, got[i].Name, want[i].ID, want[i].Name)
				}
			}
		})
	}
}

func TestCollectSandboxRefs_GoldenParity(t *testing.T) {
	for _, name := range fixtures(t) {
		t.Run(name, func(t *testing.T) {
			raw, conv := loadFixture(t, name)
			var want []goldenSandboxRef
			loadGolden(t, name, "sandbox_refs.json", &want)

			got := CollectSandboxRefs(conv, MappingOrder(raw))
			if len(got) != len(want) {
				t.Fatalf("CollectSandboxRefs = %+v, want %+v", got, want)
			}
			for i := range want {
				if got[i].MessageID != want[i].MessageID || got[i].Path != want[i].Path {
					t.Errorf("ref %d = {%s %s}, want {%s %s}",
						i, got[i].MessageID, got[i].Path, want[i].MessageID, want[i].Path)
				}
			}
		})
	}
}

func TestMappingOrder(t *testing.T) {
	raw, _ := loadFixture(t, "sandbox-unicode")
	want := []string{"root", "z1", "a2", "m3", "b4", "no-id"}
	got := MappingOrder(raw)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("MappingOrder = %v, want document order %v", got, want)
	}
}

func TestParseWithOrder(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fixtureRoot, "sandbox-unicode", "conversation.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	conv, order, err := ParseWithOrder(raw)
	if err != nil {
		t.Fatalf("ParseWithOrder: %v", err)
	}
	if title, _ := conv["title"].(string); title == "" {
		t.Error("ParseWithOrder returned a conversation without a title")
	}
	if want := "root,z1,a2,m3,b4,no-id"; strings.Join(order, ",") != want {
		t.Errorf("order = %v, want %s", order, want)
	}
	if got := ids(CollectFileRefs(conv, order)); got != "file-Zebra001,file-Alpha002,file-Beta003" {
		t.Errorf("refs collected with the paired order = %s", got)
	}

	if _, _, err := ParseWithOrder([]byte("not json")); err == nil {
		t.Error("ParseWithOrder on malformed JSON = nil error, want an error")
	}
}

func TestMappingOrder_Malformed(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"no mapping key", `{"title":"t"}`, nil},
		{"mapping is not an object", `{"mapping":[1,2]}`, nil},
		{"empty mapping", `{"mapping":{}}`, nil},
		{"repeated key keeps its first position", `{"mapping":{"b":{},"a":{},"b":{}}}`, []string{"b", "a"}},
		{"not JSON at all", `not json`, nil},
		{"top level is an array", `[{"mapping":{"a":{}}}]`, nil},
		{"truncated", `{"mapping":{"a":`, nil},
		{"mapping after other keys", `{"title":"t","x":[1,{"y":2}],"mapping":{"m1":{},"m0":{}}}`, []string{"m1", "m0"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MappingOrder([]byte(tc.raw))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("MappingOrder = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCollectFileRefs_OrderMatters shows what the order argument buys:
// without it the traversal falls back to sorted keys, which visits the
// nodes in a different sequence and so both orders and names the refs
// differently (a later attachment renames an earlier asset-pointer ref).
func TestCollectFileRefs_OrderMatters(t *testing.T) {
	raw, conv := loadFixture(t, "sandbox-unicode")
	doc := CollectFileRefs(conv, MappingOrder(raw))
	sorted := CollectFileRefs(conv, nil)

	if ids(doc) == ids(sorted) {
		t.Fatalf("fixture no longer distinguishes document order from sorted order: %v", ids(doc))
	}
	if want := "file-Zebra001,file-Alpha002,file-Beta003"; ids(doc) != want {
		t.Errorf("document order = %s, want %s", ids(doc), want)
	}
	if want := "file-Alpha002,file-Zebra001,file-Beta003"; ids(sorted) != want {
		t.Errorf("sorted fallback = %s, want %s", ids(sorted), want)
	}
	if again := CollectFileRefs(conv, nil); ids(again) != ids(sorted) {
		t.Errorf("sorted fallback is not deterministic: %s then %s", ids(sorted), ids(again))
	}
}

func ids(refs []FileRef) string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.ID
	}
	return strings.Join(out, ",")
}

// convFrom decodes a JSON literal into a conversation map.
func convFrom(t *testing.T, s string) map[string]any {
	t.Helper()
	var conv map[string]any
	if err := json.Unmarshal([]byte(s), &conv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return conv
}

func TestCollectFileRefs_Units(t *testing.T) {
	tests := []struct {
		name  string
		conv  string
		order []string
		want  string // "id=name" pairs, in order
	}{
		{
			name: "attachment name falls back to the id",
			conv: `{"mapping":{"a":{"message":{"metadata":{"attachments":[
				{"id":"f1","name":"a.txt"},{"id":"f2"},{"id":"f3","name":null},{"id":"f4","name":""}]}}}}}`,
			want: "f1=a.txt f2=f2 f3=f3 f4=f4",
		},
		{
			name: "attachments without an id are ignored",
			conv: `{"mapping":{"a":{"message":{"metadata":{"attachments":[{"name":"a.txt"},{"id":""}]}}}}}`,
			want: "",
		},
		{
			name: "asset pointers of every scheme",
			conv: `{"mapping":{"a":{"message":{"content":{"parts":[
				{"asset_pointer":"file-service://file-Abc_1"},
				{"asset_pointer":"sediment://file_00000000x"},
				{"asset_pointer":"no-slashes"},
				{"asset_pointer":null},
				{"asset_pointer":""},
				"a string part",
				null]}}}}}`,
			want: "file-Abc_1=file-Abc_1 file_00000000x=file_00000000x",
		},
		{
			name: "an asset pointer never overwrites an attachment name",
			conv: `{"mapping":{"a":{"message":{
				"metadata":{"attachments":[{"id":"file-1","name":"real.png"}]},
				"content":{"parts":[{"asset_pointer":"file-service://file-1"}]}}}}}`,
			want: "file-1=real.png",
		},
		{
			name:  "a later attachment renames an earlier ref but keeps its position",
			order: []string{"a", "b"},
			conv: `{"mapping":{
				"a":{"message":{"content":{"parts":[{"asset_pointer":"x://file-1"},{"asset_pointer":"x://file-2"}]}}},
				"b":{"message":{"metadata":{"attachments":[{"id":"file-1","name":"late.png"}]}}}}}`,
			want: "file-1=late.png file-2=file-2",
		},
		{
			name: "nodes without a message are skipped",
			conv: `{"mapping":{"a":{"message":null},"b":{},"c":{"message":{}}}}`,
			want: "",
		},
		{
			name: "no mapping",
			conv: `{"title":"t"}`,
			want: "",
		},
		{
			name: "malformed shapes are tolerated",
			conv: `{"mapping":{"a":{"message":{
				"metadata":"not an object","content":{"parts":"not a list"}}},
				"b":"not an object",
				"c":{"message":{"metadata":{"attachments":"not a list"},"content":"not an object"}},
				"d":{"message":{"content":{"parts":[{"asset_pointer":7}]}}}}}`,
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, r := range CollectFileRefs(convFrom(t, tc.conv), tc.order) {
				got = append(got, r.ID+"="+r.Name)
			}
			if strings.Join(got, " ") != tc.want {
				t.Errorf("CollectFileRefs = %q, want %q", strings.Join(got, " "), tc.want)
			}
		})
	}
}

func TestCollectSandboxRefs_Units(t *testing.T) {
	tests := []struct {
		name  string
		conv  string
		order []string
		want  string // "msgid:path" pairs, in order
	}{
		{
			name: "several links in one part keep their order",
			conv: `{"mapping":{"a":{"message":{"id":"m1","content":{"content_type":"text","parts":[
				"[a](sandbox:/mnt/data/a.csv) and [b](sandbox:/mnt/data/b.png)"]}}}}}`,
			want: "m1:/mnt/data/a.csv m1:/mnt/data/b.png",
		},
		{
			name:  "a path is reported once, for the first message",
			order: []string{"a", "b"},
			conv: `{"mapping":{
				"a":{"message":{"id":"m1","content":{"text":"sandbox:/mnt/data/x.csv"}}},
				"b":{"message":{"id":"m2","content":{"text":"sandbox:/mnt/data/x.csv again"}}}}}`,
			want: "m1:/mnt/data/x.csv",
		},
		{
			name: "messages without an id are skipped",
			conv: `{"mapping":{"a":{"message":{"content":{"text":"sandbox:/mnt/data/x.csv"}}}}}`,
			want: "",
		},
		{
			name: "links are found anywhere in the content, however nested",
			conv: `{"mapping":{"a":{"message":{"id":"m1","content":{"parts":[
				{"deep":{"deeper":["see sandbox:/mnt/data/deep.bin"]}}]}}}}}`,
			want: "m1:/mnt/data/deep.bin",
		},
		{
			name: "a link in an object key is found too, as in Python's json.dumps",
			conv: `{"mapping":{"a":{"message":{"id":"m1","content":{"sandbox:/mnt/data/key.bin":1}}}}}`,
			want: "m1:/mnt/data/key.bin",
		},
		{
			name: "the path stops at the first excluded character",
			conv: `{"mapping":{"a":{"message":{"id":"m1","content":{"parts":[
				"(sandbox:/mnt/data/paren.csv) sandbox:/mnt/data/space file.csv sandbox:/mnt/data/quote'q.csv"]}}}}}`,
			want: "m1:/mnt/data/paren.csv m1:/mnt/data/space m1:/mnt/data/quote",
		},
		{
			name: "a non-ASCII character ends the path, because json.dumps escapes it",
			conv: `{"mapping":{"a":{"message":{"id":"m1","content":{"parts":["sandbox:/mnt/data/naïve.csv"]}}}}}`,
			want: "m1:/mnt/data/na",
		},
		{
			name: "an escaped quote or backslash ends the path as well",
			conv: `{"mapping":{"a":{"message":{"id":"m1","content":{"parts":["sandbox:/mnt/data/back\\slash.csv sandbox:/mnt/data/tab\tsep.csv"]}}}}}`,
			want: "m1:/mnt/data/back m1:/mnt/data/tab",
		},
		{
			// Python dumps whatever the content is, not just objects.
			name: "a string content is scanned too",
			conv: `{"mapping":{"a":{"message":{"id":"m1","content":"plain string with sandbox:/mnt/data/str.csv"}}}}`,
			want: "m1:/mnt/data/str.csv",
		},
		{
			name: "an array content keeps element order",
			conv: `{"mapping":{"a":{"message":{"id":"m1","content":["sandbox:/mnt/data/one.csv","sandbox:/mnt/data/two.csv"]}}}}`,
			want: "m1:/mnt/data/one.csv m1:/mnt/data/two.csv",
		},
		{
			name: "nested arrays keep their order too",
			conv: `{"mapping":{"a":{"message":{"id":"m1","content":{"parts":[["sandbox:/mnt/data/nested1.csv","sandbox:/mnt/data/nested2.csv"]]}}}}}`,
			want: "m1:/mnt/data/nested1.csv m1:/mnt/data/nested2.csv",
		},
		{
			name: "a falsy content is dumped as {} and matches nothing",
			conv: `{"mapping":{"a":{"message":{"id":"m1","content":null}},"b":{"message":{"id":"m2","content":""}},"c":{"message":{"id":"m3","content":7}}}}`,
			want: "",
		},
		{
			name: "other sandbox-looking text is ignored",
			conv: `{"mapping":{"a":{"message":{"id":"m1","content":{"parts":[
				"sandbox:/etc/passwd /mnt/data/bare.csv sandbox:/mnt/dat/short.csv"]}}}}}`,
			want: "",
		},
		{
			name: "malformed shapes are tolerated",
			conv: `{"mapping":{"a":"not an object","b":{"message":7},"c":{"message":{"id":5,"content":{"text":"sandbox:/mnt/data/x"}}},"d":{"message":{"id":"m1","content":"not an object"}}}}`,
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, r := range CollectSandboxRefs(convFrom(t, tc.conv), tc.order) {
				got = append(got, r.MessageID+":"+r.Path)
			}
			if strings.Join(got, " ") != tc.want {
				t.Errorf("CollectSandboxRefs = %q, want %q", strings.Join(got, " "), tc.want)
			}
		})
	}
}

// TestAlreadyHave exercises the three ways a reference can match a file on
// disk; every expectation here was checked against the Python
// already_have() with the same directory contents.
func TestAlreadyHave(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"notes.txt", "chart", "chart.png", "12345678_photo.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	tests := []struct {
		name string
		file string
		fid  string
		want bool
	}{
		{"exact match", "notes.txt", "", true},
		{"match with an appended extension", "notes", "", true},
		{"extension-less file matches exactly", "chart", "", true},
		{"a name is not matched by a longer one", "hart.png", "", false},
		{"unknown file", "nope.bin", "", false},
		{"file id tail matches a renamed copy", "nope.bin", "abc12345678", true},
		{"a short id is used whole", "nope.bin", "0000", false},
		{"an empty id never matches", "nope.bin", "", false},
		{"directories count as present", "subdir", "", true},
		{"the name is sanitized before matching", `bad:name?.txt`, "", false},
		{"id tail wins even when the name differs", "photo.jpg", "aaaa12345678", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AlreadyHave(dir, tc.file, tc.fid); got != tc.want {
				t.Errorf("AlreadyHave(%q, %q) = %v, want %v", tc.file, tc.fid, got, tc.want)
			}
		})
	}
}

// TestAlreadyHave_ExtensionRuleNeedsTheDot pins the difference between the
// two name rules: only an added extension counts as the same file, not any
// longer name that happens to start with it.
func TestAlreadyHave_ExtensionRuleNeedsTheDot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes_final.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if AlreadyHave(dir, "notes", "") {
		t.Error(`AlreadyHave("notes") matched "notes_final.txt", want no match`)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !AlreadyHave(dir, "notes", "") {
		t.Error(`AlreadyHave("notes") missed "notes.txt", want a match`)
	}
}

func TestAlreadyHave_SanitizedNames(t *testing.T) {
	dir := t.TempDir()
	// The downloader saves under the sanitized name, so a reference whose
	// raw name contains illegal characters must still be recognized.
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("badname.txt")
	write("untitled")
	write(strings.Repeat("a", 100)) // sanitize truncates to 100 characters

	tests := []struct {
		name string
		file string
		want bool
	}{
		{"illegal characters are stripped", `bad:name?.txt`, true},
		{"whitespace runs collapse", "bad:name?.txt", true},
		{"a blank name becomes untitled", "   ", true},
		{"a name is truncated to 100 characters", strings.Repeat("a", 120), true},
		{"a different long name still misses", strings.Repeat("b", 120), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AlreadyHave(dir, tc.file, ""); got != tc.want {
				t.Errorf("AlreadyHave(%q) = %v, want %v", tc.file, got, tc.want)
			}
		})
	}
}

func TestAlreadyHave_NoDirectory(t *testing.T) {
	dir := t.TempDir()
	if AlreadyHave(filepath.Join(dir, "files"), "notes.txt", "") {
		t.Error("AlreadyHave on a missing directory = true, want false")
	}
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if AlreadyHave(file, "notes.txt", "") {
		t.Error("AlreadyHave on a plain file = true, want false")
	}
	if AlreadyHave(dir, "notes.txt", "") {
		t.Error("AlreadyHave on an unrelated directory = true, want false")
	}
}
