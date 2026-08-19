// Package files finds the files a captured ChatGPT conversation refers to
// and tells whether they are already on disk. It is a behavioral port of
// export_chats.py's collect_file_refs(), collect_sandbox_refs() and
// already_have() (export_chats.py:385-423 and 552-562); see refs_test.go
// for parity expectations, whose golden files were produced by running the
// actual Python functions.
//
// Both collectors walk every node of the conversation mapping — not just
// the current branch — and both preserve discovery order, because that is
// the order the downloader then works through and because Python's dicts
// and lists preserve it. Since Go maps have no order, the mapping key
// order is recovered separately from the raw JSON bytes by MappingOrder.
package files

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/saigyo/scrapemychats/internal/fsutil"
)

// FileRef is one uploaded/attached file the conversation refers to: the
// file id and the name to save it under. It is the Go form of one entry
// of the {file_id: suggested_name} dict Python's collect_file_refs()
// returns, kept as an ordered slice so the dict's insertion order — which
// decides download order — survives.
type FileRef struct {
	ID   string
	Name string
}

// SandboxRef is one file the code tool generated, as the (message id,
// sandbox path) pair the interpreter download route needs.
type SandboxRef struct {
	MessageID string
	Path      string
}

// assetPointerRe extracts the file id from an asset pointer such as
// "file-service://file-AbC123". Mirrors Python's re.compile(r"//([A-Za-z0-9_\-]+)").
var assetPointerRe = regexp.MustCompile(`//([A-Za-z0-9_\-]+)`)

// sandboxRe matches the sandbox links the code tool leaves in message
// text. Mirrors Python's SANDBOX_RE = re.compile(r"sandbox:(/mnt/data/[^)\"'\s\\]+)");
// \x0b is spelled out because Go's \s, unlike Python's, excludes the
// vertical tab.
var sandboxRe = regexp.MustCompile(`sandbox:(/mnt/data/[^)"'\s\x{000b}\\]+)`)

// MappingOrder returns the keys of the conversation's top-level "mapping"
// object in document order — the order Python's json.loads preserves and
// therefore the order its `for node in mapping.values()` loops visit
// nodes in. Pass the result to CollectFileRefs/CollectSandboxRefs to get
// Python's ordering; a repeated key keeps the position of its first
// occurrence (json.Unmarshal, like Python, keeps the last value).
// Malformed or mapping-less JSON yields nil, which those functions then
// fall back to sorted order for.
func MappingOrder(raw []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, _ := keyTok.(string)
		if key != "mapping" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil
			}
			continue
		}
		if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
			return nil
		}
		var keys []string
		seen := map[string]bool{}
		for dec.More() {
			nodeTok, err := dec.Token()
			if err != nil {
				return nil
			}
			nodeKey, _ := nodeTok.(string)
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil
			}
			if !seen[nodeKey] {
				seen[nodeKey] = true
				keys = append(keys, nodeKey)
			}
		}
		return keys
	}
	return nil
}

// ParseWithOrder decodes a conversation document and returns it together
// with its mapping key order, so a caller cannot end up passing the
// collectors a nil order by forgetting MappingOrder. The parsed map is
// what internal/convmd renders from as well, so one decode serves both.
func ParseWithOrder(raw []byte) (map[string]any, []string, error) {
	var conv map[string]any
	if err := json.Unmarshal(raw, &conv); err != nil {
		return nil, nil, fmt.Errorf("decode conversation JSON: %w", err)
	}
	return conv, MappingOrder(raw), nil
}

// mappingNodes returns the mapping's node objects in the given key order,
// followed by any mapping key the order didn't mention, sorted, so the
// result is deterministic even when the caller has no MappingOrder to
// hand (order == nil).
func mappingNodes(conv map[string]any, order []string) []map[string]any {
	mapping, _ := conv["mapping"].(map[string]any)
	if len(mapping) == 0 {
		return nil
	}
	nodes := make([]map[string]any, 0, len(mapping))
	used := make(map[string]bool, len(mapping))
	for _, key := range order {
		v, ok := mapping[key]
		if !ok || used[key] {
			continue
		}
		used[key] = true
		nodes = append(nodes, asMap(v))
	}
	if len(used) == len(mapping) {
		return nodes
	}
	rest := make([]string, 0, len(mapping)-len(used))
	for key := range mapping {
		if !used[key] {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	for _, key := range rest {
		nodes = append(nodes, asMap(mapping[key]))
	}
	return nodes
}

// asMap returns v as a JSON object, or an empty map if it isn't one.
func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// pyStr renders v the way Python's str() would for the scalars that reach
// these code paths; containers fall back to compact JSON.
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
		// fractional part so they match Python's int repr. The magnitude
		// guard keeps the int64 conversion (and its overflow) out of it.
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

// CollectFileRefs returns the id and suggested name of every file the
// conversation refers to, in discovery order. Port of
// export_chats.py:385-403.
//
// Two sources are scanned per message, in this order: the attachment
// entries of message metadata (id plus name, falling back to the id when
// the name is empty), and the asset pointers of content parts, whose file
// id is the identifier after "//". Ids repeat across a conversation, so
// they are deduplicated the way Python's dict is: an entry keeps the
// position of its first sighting, an attachment overwrites the name of an
// entry already seen, and an asset pointer never overwrites one (Python
// uses setdefault there).
//
// Pass MappingOrder(rawJSON) as order to visit nodes in document order,
// as Python does; nil falls back to sorted key order. ParseWithOrder does
// both in one step.
//
// Two deviations, neither reachable from a real ChatGPT payload, both
// chosen so a Go caller can keep the ids typed as strings: an attachment
// id (or, in CollectSandboxRefs, a message id) that isn't a string is
// dropped, where Python would keep it as a non-string dict key and carry
// it into the download URL; and if the document had two top-level
// "mapping" keys, the parsed map holds the last one while MappingOrder
// reports the first one's node ids, so the extra ids are ignored and the
// unmentioned ones fall to the sorted tail.
func CollectFileRefs(conv map[string]any, order []string) []FileRef {
	var refs []FileRef
	index := map[string]int{}
	for _, node := range mappingNodes(conv, order) {
		msg := asMap(node["message"])
		if len(msg) == 0 {
			continue
		}
		for _, a := range sliceOf(asMap(msg["metadata"])["attachments"]) {
			att := asMap(a)
			fid, ok := att["id"].(string)
			if !ok || fid == "" {
				continue
			}
			name := fid
			if v, ok := att["name"]; ok && truthy(v) {
				name = pyStr(v)
			}
			if i, seen := index[fid]; seen {
				refs[i].Name = name // Python: refs[fid] = ... keeps position
				continue
			}
			index[fid] = len(refs)
			refs = append(refs, FileRef{ID: fid, Name: name})
		}
		for _, p := range sliceOf(asMap(msg["content"])["parts"]) {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			ap, _ := part["asset_pointer"].(string)
			m := assetPointerRe.FindStringSubmatch(ap)
			if m == nil {
				continue
			}
			if _, seen := index[m[1]]; seen {
				continue // Python: setdefault, existing name wins
			}
			index[m[1]] = len(refs)
			refs = append(refs, FileRef{ID: m[1], Name: m[1]})
		}
	}
	return refs
}

// CollectSandboxRefs returns the (message id, sandbox path) pairs for the
// files the code tool generated, which are linked as sandbox:/mnt/data/...
// in message content. Port of export_chats.py:406-423.
//
// Python searches the JSON serialization of the whole content value
// rather than any particular field, so a link is found wherever it sits
// (parts, text, a nested tool result) and whatever the content's own JSON
// type is — an object, but equally a bare string or an array. That
// serialization is reproduced here (see contentStrings). A falsy content
// (absent, null, empty) serializes to "{}" in Python and so never
// matches. Messages without an id are skipped, and a path is reported
// once, for the first message it appears in.
//
// Pass MappingOrder(rawJSON) as order to visit nodes in document order,
// as Python does; nil falls back to sorted key order. A message id that
// isn't a string is treated as absent (see CollectFileRefs for why).
func CollectSandboxRefs(conv map[string]any, order []string) []SandboxRef {
	var refs []SandboxRef
	seen := map[string]bool{}
	for _, node := range mappingNodes(conv, order) {
		msg := asMap(node["message"])
		if len(msg) == 0 {
			continue
		}
		mid, ok := msg["id"].(string)
		if !ok || mid == "" {
			continue
		}
		for _, s := range contentStrings(msg["content"]) {
			for _, m := range sandboxRe.FindAllStringSubmatch(s, -1) {
				if seen[m[1]] {
					continue
				}
				seen[m[1]] = true
				refs = append(refs, SandboxRef{MessageID: mid, Path: m[1]})
			}
		}
	}
	return refs
}

// contentStrings returns the escaped string tokens of content in the order
// they appear in Python's json.dumps(content) — which is what
// collect_sandbox_refs() runs its regex over. Any JSON type is accepted,
// because Python dumps whatever the content is; a falsy content yields no
// tokens, matching the "{}" its `or {}` fallback dumps.
//
// Scanning the tokens instead of a reassembled blob is equivalent: the
// regex excludes the double quote that delimits them, so no match can ever
// span two tokens. Escaping matters, though, and is applied
// (pyJSONEscape): json.dumps escapes non-ASCII by default, and the
// backslash it introduces ends a match, so a path is cut at the first
// non-ASCII character exactly as in Python.
//
// The one divergence concerns object-valued content (and nested objects):
// their keys are visited in sorted rather than document order, because a
// parsed map has no order to offer. That only matters if one message
// linked two different sandbox paths from two different keys of the same
// object — array elements keep their order, and so does the relative order
// of several links inside one string, which is the common case.
func contentStrings(content any) []string {
	if !truthy(content) { // Python: `msg.get("content") or {}` -> "{}"
		return nil
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, pyJSONEscape(x))
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				out = append(out, pyJSONEscape(k))
				walk(x[k])
			}
		}
	}
	walk(content)
	return out
}

// pyJSONEscape returns the body of the JSON string literal Python's
// json.dumps() would write for s: quote and backslash escaped, control
// characters in short or \u form, and — because ensure_ascii is on by
// default — every non-ASCII character as a \uXXXX escape (a surrogate
// pair beyond the BMP).
func pyJSONEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			switch {
			case r >= 0x20 && r <= 0x7e:
				b.WriteRune(r)
			case r > 0xffff:
				r -= 0x10000
				fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			default:
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		}
	}
	return b.String()
}

// sliceOf returns v as a JSON array, or nil, mirroring `x or []`.
func sliceOf(v any) []any {
	s, _ := v.([]any)
	return s
}

// truthy reports whether v is truthy in Python's sense.
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

// AlreadyHave reports whether a file for this reference is already in
// filesDir. Port of export_chats.py:552-562.
//
// A reference matches an entry whose name equals the sanitized name, or
// starts with it plus a dot (the downloader appends an extension when the
// name had none), or contains the last 8 characters of the file id (the
// prefix a collision-renamed copy gets). Pass fid = "" when there is no
// id, as the sandbox path caller does: the id test is then disabled
// rather than matching everything, exactly as Python's "\x00" sentinel
// does. A filesDir that isn't a directory means "no".
func AlreadyHave(filesDir, name, fid string) bool {
	info, err := os.Stat(filesDir)
	if err != nil || !info.IsDir() {
		return false
	}
	entries, err := os.ReadDir(filesDir)
	if err != nil {
		return false
	}
	base := fsutil.SanitizeN(name, 100)
	tail := "\x00" // never occurs in a filename, so it never matches
	if fid != "" {
		runes := []rune(fid) // Python slices by character
		if len(runes) > 8 {
			tail = string(runes[len(runes)-8:])
		} else {
			tail = fid
		}
	}
	for _, e := range entries {
		n := e.Name()
		if n == base || strings.HasPrefix(n, base+".") || strings.Contains(n, tail) {
			return true
		}
	}
	return false
}
