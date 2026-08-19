// Package convmd renders a captured ChatGPT conversation JSON document to
// Markdown. It is a behavioral port of the pure conversation-processing
// functions of export_chats.py — ordered_messages(), part_to_text(),
// message_to_markdown() and render_markdown() (export_chats.py:292-379).
// See render_test.go for byte-for-byte parity expectations, whose golden
// files were produced by running the actual Python functions.
//
// A conversation is handled as the generic map[string]any that
// json.Unmarshal produces, because the payload is a third-party document
// whose shape varies by feature and model version; the Python original
// leans on duck typing and .get() defaults throughout. The small get*
// helpers below reproduce that tolerance: they never panic and yield the
// zero value on a type mismatch. Where Python would raise (and so abort an
// export) on input this pathological, the deviations are called out in the
// comments at the point they arise.
package convmd

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Location is the time zone used to format the create/update time lines,
// mirroring Python's time.localtime(). Production uses the machine's local
// zone; tests override it so golden files don't depend on where they were
// generated.
var Location = time.Local

// ---------------------------------------------------------------- helpers

// truthy reports whether v is truthy in Python's sense: None, False, zero
// numbers, and empty strings/lists/dicts are false, everything else true.
// The Python original tests raw JSON values this way all over
// (`if node.get("message")`, `content.get("language") or ""`, ...).
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// getStr returns m[key] when it is a string, else "".
func getStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// getMap returns m[key] when it is a JSON object, else an empty (non-nil)
// map, mirroring Python's `x.get(key) or {}`.
func getMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

// getSlice returns m[key] when it is a JSON array, else nil. Python's
// `x.get(key) or []` is looser: it also iterates a string (character by
// character) or a dict (over its keys), so a "parts" field that isn't a
// list would yield garbage sections there and nothing here. Dropping it
// is the accepted deviation; no ChatGPT payload has such a field.
func getSlice(m map[string]any, key string) []any {
	v, _ := m[key].([]any)
	return v
}

// getFloat returns m[key] as a float64 when it is a JSON number, else 0
// with ok=false. json.Number is accepted too, so conversations decoded
// with Decoder.UseNumber work as well.
func getFloat(m map[string]any, key string) (float64, bool) {
	switch x := m[key].(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case bool: // Python would happily localtime(True) -> localtime(1)
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// asMap returns v as a JSON object, or an empty map if it isn't one.
func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// pyStr renders v the way Python's str()/f-string interpolation would.
// Only the scalar cases are reachable from real conversation payloads;
// containers fall back to compact JSON (Python would print its repr, e.g.
// "{'a': 1}"), which no ChatGPT document exercises because every field
// interpolated this way is a string there.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		// JSON integers decode to float64 here; render them without a
		// fractional part so they match Python's int repr.
		if x == math.Trunc(x) && !math.IsInf(x, 0) && math.Abs(x) < 1e16 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case json.Number:
		return x.String()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// fmtGet mirrors Python's f"{m.get(key, def)}": a missing key yields def,
// while a key present with a null/number/bool value is stringified (so
// {"text": null} renders as the literal "None", exactly as Python does —
// note this differs from `m.get(key) or ""`, which the Python source uses
// in other spots and which maps null to "").
func fmtGet(m map[string]any, key, def string) string {
	v, ok := m[key]
	if !ok {
		return def
	}
	return pyStr(v)
}

// isPySpace reports whether r is one of the code points Python's
// str.strip() treats as whitespace. internal/fsutil and internal/viewer
// keep their own unexported copies of this table for the same reason:
// it's a byte-for-byte port detail, not a shared abstraction.
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

// pyCapitalize mirrors Python's str.capitalize(): title-case the first
// character, lower-case the rest. Only single-rune case mappings are
// reproduced: Python expands a first character whose title case is
// several characters (ß -> "Ss", ﬁ -> "Fi"), Go's unicode.ToTitle leaves
// it as one rune. Roles are ASCII words ("user", "assistant", "tool"),
// so no real message reaches that difference.
func pyCapitalize(s string) string {
	if s == "" {
		return ""
	}
	runes := []rune(s)
	out := make([]rune, 0, len(runes))
	out = append(out, unicode.ToTitle(runes[0]))
	for _, r := range runes[1:] {
		out = append(out, unicode.ToLower(r))
	}
	return string(out)
}

// ------------------------------------------------------------- conversion

// OrderedMessages walks the mapping tree from current_node up to the root
// via parent pointers and returns the messages it collected oldest first.
// Port of export_chats.py:292-301.
//
// Edge cases replicated: a missing (or empty/null) current_node yields no
// messages at all, because Python's `while node_id` never enters the loop
// — the root is not walked from the other end. A node id absent from
// mapping, or mapping'd to a non-object, is treated as an empty node,
// which both contributes no message and ends the walk (its parent is
// None). Nodes whose "message" is null, {} or otherwise falsy are skipped
// while the walk continues upwards.
//
// The one deliberate difference: a parent cycle makes the Python loop spin
// forever, so ids already visited end the walk here.
func OrderedMessages(conv map[string]any) []map[string]any {
	mapping := getMap(conv, "mapping")
	var chain []map[string]any
	seen := map[string]bool{}
	nodeID := conv["current_node"]
	for truthy(nodeID) {
		id, ok := nodeID.(string)
		if !ok {
			// A non-string id can never be a key of a JSON object, so
			// Python's mapping.get(id) is None: empty node, no parent.
			break
		}
		if seen[id] {
			break
		}
		seen[id] = true
		node := asMap(mapping[id])
		if msg := node["message"]; truthy(msg) {
			// A truthy non-object message is kept (Python appends it and
			// only fails later, in message_to_markdown) so that the
			// message count callers derive from this stays identical.
			chain = append(chain, asMap(msg))
		}
		nodeID = node["parent"]
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// PartToText renders one element of a content "parts" array.
// Port of export_chats.py:304-316.
//
// The shapes handled, in the order Python tests them:
//   - a bare string part: returned as is;
//   - an object with an "asset_pointer" key (images, files): "[image: <ptr>]",
//     checked before content_type, so it wins for any part carrying one;
//   - content_type "audio_transcription": its "text" ("" when absent);
//   - any object with a "text" key: the text, or "" when it is null/empty;
//   - anything else: "[<content_type>]", or "[unsupported content]" when
//     content_type is absent/empty;
//   - any other JSON type (number, null, array): "".
func PartToText(part any) string {
	switch p := part.(type) {
	case string:
		return p
	case map[string]any:
		ct := p["content_type"] // Python: .get("content_type", "")
		if v, ok := p["asset_pointer"]; ok {
			return "[image: " + pyStr(v) + "]"
		}
		if s, ok := ct.(string); ok && s == "audio_transcription" {
			v, ok := p["text"]
			if !ok || v == nil {
				// Python returns None here for an explicit null; callers
				// filter falsy results, so "" is equivalent.
				return ""
			}
			return pyStr(v)
		}
		if v, ok := p["text"]; ok {
			if !truthy(v) {
				return ""
			}
			return pyStr(v)
		}
		if truthy(ct) {
			return "[" + pyStr(ct) + "]"
		}
		return "[unsupported content]"
	}
	return ""
}

// MessageToMarkdown renders one message as a "## <Role>" section, or
// reports ok=false when the message is one the Python original drops.
// Port of export_chats.py:319-362.
//
// Dropped messages: those flagged
// metadata.is_visually_hidden_from_conversation; role "system"; content
// types user_editable_context and model_editable_context; messages
// addressed to a tool (a "recipient" other than null or "all", which are
// hidden in the UI); and messages whose rendered body is blank.
//
// Bodies by content type: text and multimodal_text join their non-empty
// parts with a blank line; code wraps content.text in a fence tagged with
// content.language; execution_output wraps it in a bare fence; thoughts
// renders each entry as "> <summary>: <content>"; tether_quote renders
// title and text as a two-line quote; every other content type (including
// reasoning_recap) falls back to content.text, then content.result, then
// the literal "[<content_type>]".
func MessageToMarkdown(msg map[string]any) (string, bool) {
	meta := getMap(msg, "metadata")
	if truthy(meta["is_visually_hidden_from_conversation"]) {
		return "", false
	}
	author := getMap(msg, "author")
	role := "?" // Python's .get("role", "?") default
	if v, ok := author["role"]; ok {
		// An explicit null role makes Python raise on .capitalize(); we
		// render it as the string "None" rather than abort the export.
		role = pyStr(v)
	}
	name := author["name"]
	content := getMap(msg, "content")
	ctv := content["content_type"] // may be absent -> None
	ct := getStr(content, "content_type")

	if role == "system" || ct == "user_editable_context" || ct == "model_editable_context" {
		return "", false
	}
	if r, ok := msg["recipient"]; ok && r != nil {
		if s, isStr := r.(string); !isStr || s != "all" {
			return "", false
		}
	}

	var body string
	switch ct {
	case "text", "multimodal_text":
		var parts []string
		for _, p := range getSlice(content, "parts") {
			if t := PartToText(p); t != "" {
				parts = append(parts, t)
			}
		}
		body = strings.Join(parts, "\n\n")
	case "code":
		lang := ""
		if v := content["language"]; truthy(v) {
			lang = pyStr(v)
		}
		body = "```" + lang + "\n" + fmtGet(content, "text", "") + "\n```"
	case "execution_output":
		body = "```\n" + fmtGet(content, "text", "") + "\n```"
	case "thoughts":
		var thoughts []string
		for _, t := range getSlice(content, "thoughts") {
			tm := asMap(t)
			thoughts = append(thoughts, "> "+fmtGet(tm, "summary", "")+": "+fmtGet(tm, "content", ""))
		}
		body = strings.Join(thoughts, "\n\n")
	case "tether_quote":
		body = "> " + fmtGet(content, "title", "") + "\n> " + fmtGet(content, "text", "")
	default:
		switch {
		case truthy(content["text"]):
			body = pyStr(content["text"])
		case truthy(content["result"]):
			body = pyStr(content["result"])
		default:
			// f"[{ct}]" on the raw value: an absent content_type prints
			// as "[None]".
			body = "[" + pyStr(ctv) + "]"
		}
	}

	if pyStrip(body) == "" {
		return "", false
	}

	// Attachments are prepended only after the blank-body check, so a
	// message that carries attachments but no text is still dropped.
	if attachments := getSlice(meta, "attachments"); len(attachments) > 0 {
		var att []string
		for _, a := range attachments {
			am := asMap(a)
			// Python: a.get("name", a.get("id")) — the id is the default
			// for an absent "name", but a present name of null stays null
			// ("None"), and an absent id makes that default None too.
			v, ok := am["name"]
			if !ok {
				v = am["id"]
			}
			att = append(att, "- attached: `"+pyStr(v)+"`")
		}
		body = strings.Join(att, "\n") + "\n\n" + body
	}

	header := pyCapitalize(role)
	if role == "tool" {
		n := "tool"
		if truthy(name) {
			n = pyStr(name)
		}
		header = "Tool (" + n + ")"
	}
	return "## " + header + "\n\n" + body, true
}

// RenderMarkdown renders the whole conversation document: a title line
// (the conversation's own title, falling back to the title from the chat
// list), the source URL, the create/update times formatted in Location,
// then one section per rendered message. Port of export_chats.py:365-379.
//
// Timestamps are unix seconds as floats; Python's time.localtime() floors
// them (it rounds toward minus infinity, not toward zero), and a falsy
// timestamp — absent, null or exactly 0 — omits the line entirely.
func RenderMarkdown(conv map[string]any, url, title string) string {
	head := title
	if v, ok := conv["title"]; ok && truthy(v) {
		head = pyStr(v)
	}
	lines := []string{"# " + head, "", "- URL: " + url}
	for _, key := range []string{"create_time", "update_time"} {
		if !truthy(conv[key]) {
			continue
		}
		ts, ok := getFloat(conv, key)
		if !ok {
			// Python's time.localtime() would raise TypeError on a
			// non-number; skip the line rather than abort the export.
			continue
		}
		stamp := time.Unix(int64(math.Floor(ts)), 0).In(Location).Format("2006-01-02 15:04:05")
		lines = append(lines, "- "+strings.ReplaceAll(key, "_", " ")+": "+stamp)
	}
	lines = append(lines, "")
	for _, msg := range OrderedMessages(conv) {
		if md, ok := MessageToMarkdown(msg); ok {
			lines = append(lines, md, "")
		}
	}
	return strings.Join(lines, "\n")
}
