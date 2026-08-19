package convmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	// The golden Markdown carries local-time stamps generated with
	// TZ=Europe/Berlin, so the zone database has to be available even on a
	// machine that has none installed (a bare container, say).
	_ "time/tzdata"
)

// goldenTZ is the zone the golden files were generated in. The goldens
// under testdata (conversation.md, file_refs.json, sandbox_refs.json) come
// from running export_chats.py's own render_markdown(), collect_file_refs()
// and collect_sandbox_refs() over each conversation.json, with the zone
// pinned so the local-time stamps don't depend on the generating machine:
//
//	TZ=Europe/Berlin python3 -c 'import json, export_chats as ec; ...'
//
// (export_chats.py imports playwright at module scope, so generating them
// needs playwright installed or a two-file stub package on PYTHONPATH
// providing sync_api.TimeoutError and sync_api.sync_playwright.)
const goldenTZ = "Europe/Berlin"

// Test-only URL/title arguments, derived from the fixture directory name
// exactly as the generator script does.
const (
	urlPrefix   = "https://chatgpt.com/c/"
	titleSuffix = " (title from chats.csv)"
)

func TestMain(m *testing.M) {
	loc, err := time.LoadLocation(goldenTZ)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot load %s: %v\n", goldenTZ, err)
		os.Exit(1)
	}
	Location = loc
	os.Exit(m.Run())
}

// fixtures lists the fixture directory names under testdata.
func fixtures(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("no fixtures found in testdata")
	}
	return names
}

// loadFixture decodes testdata/<name>/conversation.json.
func loadFixture(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "conversation.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var conv map[string]any
	if err := json.Unmarshal(raw, &conv); err != nil {
		t.Fatalf("decode fixture %s: %v", dir, err)
	}
	return conv
}

// TestRenderMarkdown_GoldenParity renders every fixture and compares the
// result byte for byte with the Markdown the Python render_markdown()
// produced for the same input.
func TestRenderMarkdown_GoldenParity(t *testing.T) {
	for _, name := range fixtures(t) {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("testdata", name)
			conv := loadFixture(t, dir)
			want, err := os.ReadFile(filepath.Join(dir, "conversation.md"))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			got := RenderMarkdown(conv, urlPrefix+name, name+titleSuffix)
			if got != string(want) {
				t.Errorf("rendered Markdown differs from the Python golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

// TestOrderedMessages_GoldenCounts pins the number of messages the walk
// collects per fixture, which is the count export_chats.py writes to
// manifest.csv. The expectations were printed by the generator script
// running the Python ordered_messages().
func TestOrderedMessages_GoldenCounts(t *testing.T) {
	want := map[string]int{
		"branched":        6,
		"code-exec":       6,
		"content-types":   10,
		"edge":            0,
		"multimodal":      3,
		"plain":           2,
		"sandbox-unicode": 4,
	}
	for _, name := range fixtures(t) {
		t.Run(name, func(t *testing.T) {
			n, ok := want[name]
			if !ok {
				t.Fatalf("fixture %q has no expected message count; add one", name)
			}
			if got := len(OrderedMessages(loadFixture(t, filepath.Join("testdata", name)))); got != n {
				t.Errorf("OrderedMessages = %d messages, want %d", got, n)
			}
		})
	}
}

// convFrom decodes a JSON literal into the generic map the renderer takes.
func convFrom(t *testing.T, s string) map[string]any {
	t.Helper()
	var conv map[string]any
	if err := json.Unmarshal([]byte(s), &conv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return conv
}

// msgIDs returns the "id" of each message the walk collected.
func msgIDs(msgs []map[string]any) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		s, _ := m["id"].(string)
		out = append(out, s)
	}
	return out
}

func TestOrderedMessages_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		conv string
		want []string
	}{
		{
			name: "oldest first",
			conv: `{"current_node":"c","mapping":{
				"a":{"message":{"id":"1"},"parent":null},
				"b":{"message":{"id":"2"},"parent":"a"},
				"c":{"message":{"id":"3"},"parent":"b"}}}`,
			want: []string{"1", "2", "3"},
		},
		{"no current_node", `{"mapping":{"a":{"message":{"id":"1"}}}}`, nil},
		{"null current_node", `{"current_node":null,"mapping":{"a":{"message":{"id":"1"}}}}`, nil},
		{"empty current_node", `{"current_node":"","mapping":{"a":{"message":{"id":"1"}}}}`, nil},
		{"current_node not in mapping", `{"current_node":"nope","mapping":{"a":{"message":{"id":"1"}}}}`, nil},
		{"no mapping at all", `{"current_node":"a"}`, nil},
		{"non-string current_node", `{"current_node":7,"mapping":{"a":{"message":{"id":"1"}}}}`, nil},
		{
			name: "null and empty messages are skipped, walk continues",
			conv: `{"current_node":"c","mapping":{
				"a":{"message":{"id":"1"},"parent":null},
				"b":{"message":null,"parent":"a"},
				"c":{"message":{},"parent":"b"}}}`,
			want: []string{"1"},
		},
		{
			name: "parent missing from mapping ends the walk",
			conv: `{"current_node":"b","mapping":{
				"b":{"message":{"id":"2"},"parent":"gone"}}}`,
			want: []string{"2"},
		},
		{
			name: "mapping value of the wrong type is an empty node",
			conv: `{"current_node":"a","mapping":{"a":"not an object"}}`,
			want: nil,
		},
		{
			name: "mapping of the wrong type",
			conv: `{"current_node":"a","mapping":[1,2,3]}`,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := msgIDs(OrderedMessages(convFrom(t, tc.conv)))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("OrderedMessages = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOrderedMessages_ParentCycle covers the one deliberate difference
// from Python, which would loop forever on a cycle.
func TestOrderedMessages_ParentCycle(t *testing.T) {
	conv := convFrom(t, `{"current_node":"a","mapping":{
		"a":{"message":{"id":"1"},"parent":"b"},
		"b":{"message":{"id":"2"},"parent":"a"}}}`)
	done := make(chan []string, 1)
	go func() { done <- msgIDs(OrderedMessages(conv)) }()
	select {
	case got := <-done:
		if len(got) != 2 {
			t.Errorf("OrderedMessages = %v, want both messages once", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OrderedMessages did not terminate on a parent cycle")
	}
}

func TestPartToText(t *testing.T) {
	tests := []struct {
		name string
		part string
		want string
	}{
		{"bare string", `"just text"`, "just text"},
		{"empty string", `""`, ""},
		{"asset pointer", `{"content_type":"image_asset_pointer","asset_pointer":"file-service://file-Abc"}`, "[image: file-service://file-Abc]"},
		{"asset pointer wins over text", `{"content_type":"text","text":"ignored","asset_pointer":"sediment://file_1"}`, "[image: sediment://file_1]"},
		{"null asset pointer still matches the key", `{"asset_pointer":null}`, "[image: None]"},
		{"audio transcription", `{"content_type":"audio_transcription","text":"spoken words"}`, "spoken words"},
		{"audio transcription without text", `{"content_type":"audio_transcription"}`, ""},
		{"audio transcription with null text", `{"content_type":"audio_transcription","text":null}`, ""},
		{"text key", `{"content_type":"text","text":"body"}`, "body"},
		{"null text", `{"content_type":"text","text":null}`, ""},
		{"empty text", `{"content_type":"text","text":""}`, ""},
		{"unknown content type", `{"content_type":"real_time_user_audio_video_asset_pointer"}`, "[real_time_user_audio_video_asset_pointer]"},
		{"no content type", `{"other":1}`, "[unsupported content]"},
		{"empty content type", `{"content_type":""}`, "[unsupported content]"},
		{"null content type", `{"content_type":null}`, "[unsupported content]"},
		{"number part", `42`, ""},
		{"null part", `null`, ""},
		{"array part", `[1,2]`, ""},
		{"bool part", `true`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var part any
			if err := json.Unmarshal([]byte(tc.part), &part); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := PartToText(part); got != tc.want {
				t.Errorf("PartToText(%s) = %q, want %q", tc.part, got, tc.want)
			}
		})
	}
}

// msgFrom decodes a JSON message literal.
func msgFrom(t *testing.T, s string) map[string]any {
	t.Helper()
	var msg map[string]any
	if err := json.Unmarshal([]byte(s), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return msg
}

func TestMessageToMarkdown_Skipped(t *testing.T) {
	tests := []struct{ name, msg string }{
		{"visually hidden", `{"author":{"role":"assistant"},"content":{"content_type":"text","parts":["x"]},"metadata":{"is_visually_hidden_from_conversation":true}}`},
		{"system role", `{"author":{"role":"system"},"content":{"content_type":"text","parts":["x"]}}`},
		{"user editable context", `{"author":{"role":"user"},"content":{"content_type":"user_editable_context","user_profile":"x"}}`},
		{"model editable context", `{"author":{"role":"user"},"content":{"content_type":"model_editable_context","text":"x"}}`},
		{"addressed to a tool", `{"author":{"role":"assistant"},"recipient":"python","content":{"content_type":"text","parts":["x"]}}`},
		{"empty recipient is not \"all\"", `{"author":{"role":"assistant"},"recipient":"","content":{"content_type":"text","parts":["x"]}}`},
		{"non-string recipient", `{"author":{"role":"assistant"},"recipient":5,"content":{"content_type":"text","parts":["x"]}}`},
		{"blank body", `{"author":{"role":"assistant"},"content":{"content_type":"text","parts":["","  \n "]}}`},
		{"blank body wins over attachments", `{"author":{"role":"user"},"content":{"content_type":"text","parts":[""]},"metadata":{"attachments":[{"id":"file-1","name":"a.txt"}]}}`},
		{"no parts", `{"author":{"role":"user"},"content":{"content_type":"text"}}`},
		{"empty thoughts", `{"author":{"role":"assistant"},"content":{"content_type":"thoughts","thoughts":[]}}`},
		// Python's str.strip() removes the C0 separators U+001C-U+001F,
		// which unicode.IsSpace does not consider whitespace.
		{"a body of only C0 separators is blank", `{"author":{"role":"user"},"content":{"content_type":"text","parts":["\u001f\u001c"]}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := MessageToMarkdown(msgFrom(t, tc.msg)); ok {
				t.Errorf("MessageToMarkdown = %q, want it to be skipped", got)
			}
		})
	}
}

func TestMessageToMarkdown_Rendered(t *testing.T) {
	tests := []struct{ name, msg, want string }{
		{
			"missing recipient is allowed",
			`{"author":{"role":"user"},"content":{"content_type":"text","parts":["hi"]}}`,
			"## User\n\nhi",
		},
		{
			"null recipient is allowed",
			`{"author":{"role":"assistant"},"recipient":null,"content":{"content_type":"text","parts":["hi"]}}`,
			"## Assistant\n\nhi",
		},
		{
			"missing role",
			`{"author":{},"content":{"content_type":"text","parts":["hi"]}}`,
			"## ?\n\nhi",
		},
		{
			"missing author",
			`{"content":{"content_type":"text","parts":["hi"]}}`,
			"## ?\n\nhi",
		},
		{
			"role is lower-cased after the first character",
			`{"author":{"role":"aSSISTANT"},"content":{"content_type":"text","parts":["hi"]}}`,
			"## Assistant\n\nhi",
		},
		{
			"tool without a name",
			`{"author":{"role":"tool"},"content":{"content_type":"execution_output","text":"out"}}`,
			"## Tool (tool)\n\n```\nout\n```",
		},
		{
			"tool with a name",
			`{"author":{"role":"tool","name":"browser"},"content":{"content_type":"execution_output","text":"out"}}`,
			"## Tool (browser)\n\n```\nout\n```",
		},
		{
			"code without a language",
			`{"author":{"role":"assistant"},"content":{"content_type":"code","text":"1+1"}}`,
			"## Assistant\n\n```\n1+1\n```",
		},
		{
			"code with a null text renders the literal None",
			`{"author":{"role":"assistant"},"content":{"content_type":"code","language":"py","text":null}}`,
			"## Assistant\n\n```py\nNone\n```",
		},
		{
			"execution_output with a null text renders the literal None",
			`{"author":{"role":"tool","name":"python"},"content":{"content_type":"execution_output","text":null}}`,
			"## Tool (python)\n\n```\nNone\n```",
		},
		{
			"a C0 separator inside a body is kept, only a blank body is dropped",
			`{"author":{"role":"user"},"content":{"content_type":"text","parts":["\u001fx"]}}`,
			"## User\n\n\x1fx",
		},
		{
			"code with a missing text renders empty",
			`{"author":{"role":"assistant"},"content":{"content_type":"code","language":"py","other":"x"}}`,
			"## Assistant\n\n```py\n\n```",
		},
		{
			// The fence characters keep the body non-blank, so an empty
			// code block is rendered rather than dropped.
			"empty code block",
			`{"author":{"role":"assistant"},"content":{"content_type":"code","language":"","text":""}}`,
			"## Assistant\n\n```\n\n```",
		},
		{
			"thoughts",
			`{"author":{"role":"assistant"},"content":{"content_type":"thoughts","thoughts":[{"summary":"s1","content":"c1"},{"content":"c2"}]}}`,
			"## Assistant\n\n> s1: c1\n\n> : c2",
		},
		{
			"tether quote",
			`{"author":{"role":"tool","name":"browser"},"content":{"content_type":"tether_quote","title":"T","text":"Q"}}`,
			"## Tool (browser)\n\n> T\n> Q",
		},
		{
			"tether quote with missing fields",
			`{"author":{"role":"tool","name":"browser"},"content":{"content_type":"tether_quote","url":"u"}}`,
			"## Tool (browser)\n\n> \n> ",
		},
		{
			"unknown content type falls back to text",
			`{"author":{"role":"assistant"},"content":{"content_type":"sonic_webpage","text":"page text"}}`,
			"## Assistant\n\npage text",
		},
		{
			"unknown content type falls back to result",
			`{"author":{"role":"tool","name":"browser"},"content":{"content_type":"tether_browsing_display","result":"res"}}`,
			"## Tool (browser)\n\nres",
		},
		{
			"unknown content type with neither prints the type",
			`{"author":{"role":"assistant"},"content":{"content_type":"reasoning_recap","content":"Thought for 3s"}}`,
			"## Assistant\n\n[reasoning_recap]",
		},
		{
			"missing content type prints None",
			`{"author":{"role":"assistant"},"content":{"parts":["ignored"]}}`,
			"## Assistant\n\n[None]",
		},
		{
			"multimodal parts are joined with a blank line, empties dropped",
			`{"author":{"role":"user"},"content":{"content_type":"multimodal_text","parts":["a","",{"asset_pointer":"x//file-1"},"b"]}}`,
			"## User\n\na\n\n[image: x//file-1]\n\nb",
		},
		{
			"attachments are listed before the body",
			`{"author":{"role":"user"},"content":{"content_type":"text","parts":["look"]},"metadata":{"attachments":[{"id":"file-1","name":"a.txt"},{"id":"file-2"},{"id":"file-3","name":null},{"name":"orphan"},{}]}}`,
			"## User\n\n- attached: `a.txt`\n- attached: `file-2`\n- attached: `None`\n- attached: `orphan`\n- attached: `None`\n\nlook",
		},
		{
			"empty attachment list adds nothing",
			`{"author":{"role":"user"},"content":{"content_type":"text","parts":["look"]},"metadata":{"attachments":[]}}`,
			"## User\n\nlook",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := MessageToMarkdown(msgFrom(t, tc.msg))
			if !ok {
				t.Fatalf("MessageToMarkdown skipped the message, want %q", tc.want)
			}
			if got != tc.want {
				t.Errorf("MessageToMarkdown = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMessageToMarkdown_MalformedInput checks the duck-typing tolerance:
// none of these shapes may panic.
func TestMessageToMarkdown_MalformedInput(t *testing.T) {
	msgs := []string{
		`{}`,
		`{"author":"not an object","content":"not an object","metadata":"not an object"}`,
		`{"author":{"role":42},"content":{"content_type":"text","parts":"not a list"}}`,
		`{"author":{"role":"user"},"content":{"content_type":7,"text":"x"}}`,
		`{"author":{"role":"user"},"content":{"content_type":"thoughts","thoughts":["not an object",null]}}`,
		`{"author":{"role":"user"},"content":{"content_type":"text","parts":[null,7,[1]]},"metadata":{"attachments":["not an object"]}}`,
		`{"author":{"role":"user"},"metadata":{"attachments":"not a list"},"content":{"content_type":"text","parts":["x"]}}`,
		`{"recipient":["not","a","string"],"content":{"content_type":"text","parts":["x"]}}`,
	}
	for i, m := range msgs {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			MessageToMarkdown(msgFrom(t, m)) // must not panic
		})
	}
}

func TestRenderMarkdown_Header(t *testing.T) {
	tests := []struct{ name, conv, want string }{
		{
			"conversation title wins",
			`{"title":"Real title"}`,
			"# Real title\n\n- URL: u\n",
		},
		{
			"empty title falls back to the chat list title",
			`{"title":""}`,
			"# csv title\n\n- URL: u\n",
		},
		{
			"null title falls back",
			`{"title":null}`,
			"# csv title\n\n- URL: u\n",
		},
		{
			"missing title falls back",
			`{}`,
			"# csv title\n\n- URL: u\n",
		},
		{
			"timestamps are floored, not rounded",
			`{"title":"t","create_time":1700000000.999,"update_time":1700000001.001}`,
			"# t\n\n- URL: u\n- create time: 2023-11-14 23:13:20\n- update time: 2023-11-14 23:13:21\n",
		},
		{
			"a zero create_time is dropped",
			`{"title":"t","create_time":0,"update_time":1700000001}`,
			"# t\n\n- URL: u\n- update time: 2023-11-14 23:13:21\n",
		},
		{
			"null and missing timestamps are dropped",
			`{"title":"t","create_time":null}`,
			"# t\n\n- URL: u\n",
		},
		{
			"a non-numeric timestamp is dropped rather than crashing",
			`{"title":"t","create_time":"yesterday","update_time":1700000001}`,
			"# t\n\n- URL: u\n- update time: 2023-11-14 23:13:21\n",
		},
		{
			"negative timestamps floor towards minus infinity",
			`{"title":"t","create_time":-0.5}`,
			"# t\n\n- URL: u\n- create time: 1970-01-01 00:59:59\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderMarkdown(convFrom(t, tc.conv), "u", "csv title"); got != tc.want {
				t.Errorf("RenderMarkdown = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLocationIsInjectable documents that production formats in the
// machine's local zone while tests pin a zone; the golden files depend on
// it, so a regression here would be invisible otherwise.
func TestLocationIsInjectable(t *testing.T) {
	conv := convFrom(t, `{"title":"t","create_time":1700000000}`)
	saved := Location
	defer func() { Location = saved }()

	Location = time.UTC
	utc := RenderMarkdown(conv, "u", "csv")
	Location = saved
	berlin := RenderMarkdown(conv, "u", "csv")

	if !strings.Contains(utc, "22:13:20") {
		t.Errorf("UTC rendering = %q, want 22:13:20", utc)
	}
	if !strings.Contains(berlin, "23:13:20") {
		t.Errorf("%s rendering = %q, want 23:13:20", goldenTZ, berlin)
	}
}
