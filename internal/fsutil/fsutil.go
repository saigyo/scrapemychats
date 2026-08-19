// Package fsutil provides small filesystem-adjacent helpers shared by the
// scrapemychats commands: making arbitrary strings safe as file/folder
// names, and reading CSV files that may carry a UTF-8 BOM (e.g. because a
// user opened chats.csv in Excel and re-saved it).
//
// Sanitize is a byte-for-byte behavioral port of export_chats.py's
// sanitize() (export_chats.py:62-66); see fsutil_test.go for expectations
// verified against the actual Python implementation.
package fsutil

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// DefaultMaxLen is the default max_len used by the Python sanitize().
const DefaultMaxLen = 60

// illegalCharsRe matches characters that are unsafe in file/folder names on
// any OS, plus ASCII control characters. Mirrors Python's
// re.compile(r'[<>:"/\\|?*\x00-\x1f]').
var illegalCharsRe = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

// whitespaceRunRe matches a run of whitespace characters, mirroring the set
// matched by Python's \s in Unicode mode: ASCII whitespace plus the
// Unicode White_Space code points that survive illegalCharsRe (everything
// below U+0020 is already stripped by that pass, so only U+0020 and the
// non-control Unicode space separators/line separators remain relevant).
var whitespaceRunRe = regexp.MustCompile(`[\s\x{0085}\x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]+`)

// isPySpace reports whether r is one of the code points Python's str.strip()
// (and re's \s in Unicode mode) treats as whitespace. This is the exact set
// CPython's Py_UNICODE_ISSPACE table uses.
func isPySpace(r rune) bool {
	switch r {
	case 0x09, 0x0A, 0x0B, 0x0C, 0x0D, // \t \n \v \f \r
		0x1C, 0x1D, 0x1E, 0x1F, // file/group/record/unit separators
		0x20,   // space
		0x85,   // NEL
		0xA0,   // NBSP
		0x1680, // OGHAM SPACE MARK
		0x2028, // LINE SEPARATOR
		0x2029, // PARAGRAPH SEPARATOR
		0x202F, // NARROW NO-BREAK SPACE
		0x205F, // MEDIUM MATHEMATICAL SPACE
		0x3000: // IDEOGRAPHIC SPACE
		return true
	}
	return r >= 0x2000 && r <= 0x200A // EN QUAD .. HAIR SPACE
}

// pyStrip trims leading and trailing Python-whitespace runes from s,
// matching Python's str.strip() with no arguments.
func pyStrip(s string) string {
	return strings.TrimFunc(s, isPySpace)
}

// Sanitize makes a string safe for use as a file/folder name on any OS,
// using the default max length of 60 characters (runes). It is a
// behavioral port of export_chats.py's sanitize(name, max_len=60).
func Sanitize(name string) string {
	return SanitizeN(name, DefaultMaxLen)
}

// SanitizeN is Sanitize with an explicit max length, in runes (characters),
// matching Python's name[:max_len] which slices by character, not byte.
// A negative maxLen is treated as 0 (Python's negative-slice semantics are
// not replicated; no call site uses one).
func SanitizeN(name string, maxLen int) string {
	if maxLen < 0 {
		maxLen = 0
	}
	name = illegalCharsRe.ReplaceAllString(name, "")
	name = whitespaceRunRe.ReplaceAllString(name, " ")
	name = pyStrip(name)
	name = strings.Trim(name, ". ")

	runes := []rune(name)
	if len(runes) > maxLen {
		runes = runes[:maxLen]
	}
	name = pyStrip(string(runes))

	if name == "" {
		return "untitled"
	}
	// On Windows, a basename whose stem is a reserved device name (CON, NUL,
	// COM1…) can't be created at all, so an attachment named e.g. "CON.txt"
	// would fail the export. Escape it there only, so macOS/Linux output stays
	// byte-identical to the Python tool (which doesn't handle these either).
	if runtime.GOOS == "windows" {
		name = escapeReservedName(name)
	}
	return name
}

// reservedWindowsNames are the device names Windows refuses to use as a file
// basename, with or without an extension (case-insensitive).
var reservedWindowsNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// escapeReservedName prefixes an underscore when name's stem (the part before
// the first dot) is a Windows reserved device name, making it creatable on
// Windows. It is a no-op for every non-reserved name, so ordinary
// archive-compatible names are unchanged.
func escapeReservedName(name string) string {
	stem := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		stem = name[:i]
	}
	if reservedWindowsNames[strings.ToUpper(stem)] {
		return "_" + name
	}
	return name
}

// WriteFileAtomic writes data to path via a temp file in the same directory
// and an atomic rename, so a reader — or a resume/existence check — only ever
// sees the file absent or fully written, never truncated. Used for every file
// whose partial presence would be mistaken for a complete one: the resume
// marker, the chat list, and downloaded attachments. The temp name is
// dot-prefixed so a rare leftover (only if cleanup itself fails) is skipped by
// the viewer's per-chat file scan.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".smc-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false // renamed into place; nothing to remove
	return nil
}

// bomBytes is the UTF-8 encoding of U+FEFF, the byte order mark.
var bomBytes = []byte{0xEF, 0xBB, 0xBF}

// bomStrippingReader strips a single leading UTF-8 BOM, if present, from the
// wrapped reader's output. This mirrors Python's encoding="utf-8-sig",
// which export_chats.py uses when reading chats.csv (NOT manifest.csv,
// which Python reads as plain utf-8) so that files re-saved by tools like
// Excel (which prepend a BOM) still parse.
type bomStrippingReader struct {
	r       *bufio.Reader
	checked bool
}

func (b *bomStrippingReader) Read(p []byte) (int, error) {
	if !b.checked {
		b.checked = true
		head, err := b.r.Peek(len(bomBytes))
		if err == nil && string(head) == string(bomBytes) {
			if _, discardErr := b.r.Discard(len(bomBytes)); discardErr != nil {
				return 0, discardErr
			}
		}
		// A short read (err != nil, e.g. io.EOF because the file is shorter
		// than the BOM) just means there's no BOM to strip; fall through
		// and let the normal Read report the underlying condition.
	}
	return b.r.Read(p)
}

// NewBOMTolerantReader wraps r so a leading UTF-8 BOM (as written by tools
// such as Excel) is transparently skipped, then returns a csv.Reader over
// the result. This matches how export_chats.py opens chats.csv with
// encoding="utf-8-sig" for reading (manifest.csv is read as plain utf-8);
// scrapemychats never writes a BOM itself (Python writes with
// encoding="utf-8"), only tolerates one on read. FieldsPerRecord is
// disabled because Python's csv.reader allows ragged rows and
// export_chats.py relies on that (e.g. `row[1] if len(row) > 1 else ""`).
func NewBOMTolerantReader(r io.Reader) *csv.Reader {
	cr := csv.NewReader(&bomStrippingReader{r: bufio.NewReader(r)})
	cr.FieldsPerRecord = -1
	return cr
}

// Log prints msg followed by a newline and flushes immediately, mirroring
// export_chats.py's log(msg) = print(msg, flush=True). Kept as a thin
// wrapper purely for call-site parity with the Python source.
func Log(msg string) {
	fmt.Println(msg)
}
