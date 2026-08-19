package fsutil

import (
	"strings"
	"testing"
)

// Expected values below were derived by running the actual Python
// implementation (export_chats.py:62-66) via:
//
//	python3 -c '
//	import re
//	def sanitize(name, max_len=60):
//	    name = re.sub(r"[<>:\"/\\\\|?*\x00-\x1f]", "", name)
//	    name = re.sub(r"\s+", " ", name).strip().strip(". ")
//	    return name[:max_len].strip() or "untitled"
//	print(repr(sanitize(INPUT)))
//	'
//
// so they are ground truth, not guesses.
func TestSanitize(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "illegal chars removed",
			input: `My<>:"/\|?*Title`,
			want:  "MyTitle",
		},
		{
			name:  "control chars removed without leaving a gap",
			input: "Hello\x01\x02World\x1f",
			want:  "HelloWorld",
		},
		{
			name:  "ascii whitespace runs collapse to one space",
			input: "Hello  World  \t Test",
			want:  "Hello World Test",
		},
		{
			name:  "non-breaking space between words collapses like ascii space",
			input: "Hello World",
			want:  "Hello World",
		},
		{
			name:  "leading/trailing non-breaking spaces are stripped",
			input: "  Hello World  ",
			want:  "Hello World",
		},
		{
			name:  "leading/trailing ideographic spaces are stripped",
			input: "　Hello　World　",
			want:  "Hello World",
		},
		{
			name: "unicode space collapses to one space but a removed " +
				"control char leaves no space at all",
			input: "Hello  World\t\tTest",
			want:  "Hello WorldTest",
		},
		{
			name:  "leading/trailing dots and spaces stripped",
			input: "  ...Hello World...  ",
			want:  "Hello World",
		},
		{
			name:  "alternating dots and spaces stripped from both ends",
			input: ". . .Hello World. . .",
			want:  "Hello World",
		},
		{
			name:  "empty string falls back to untitled",
			input: "",
			want:  "untitled",
		},
		{
			name:  "only illegal chars falls back to untitled",
			input: `<>:"/\|?*`,
			want:  "untitled",
		},
		{
			name:  "only whitespace falls back to untitled",
			input: "   \t\n  ",
			want:  "untitled",
		},
		{
			name:  "only dots and spaces falls back to untitled",
			input: " . . . ",
			want:  "untitled",
		},
		{
			name:  "exactly 60 ascii chars is unchanged",
			input: strings.Repeat("a", 60),
			want:  strings.Repeat("a", 60),
		},
		{
			name:  "61 ascii chars truncated to 60",
			input: strings.Repeat("a", 61),
			want:  strings.Repeat("a", 60),
		},
		{
			name:  "rune-safe truncation of multibyte CJK text at 60 runes",
			input: strings.Repeat("日本語", 30), // 90 runes
			want:  strings.Repeat("日本語", 20), // 60 runes
		},
		{
			name:  "rune-safe truncation of multibyte emoji at 60 runes",
			input: strings.Repeat("\U0001F600", 70),
			want:  strings.Repeat("\U0001F600", 60),
		},
		{
			name:  "truncation that lands on a space trims it via the final strip",
			input: strings.Repeat("a", 59) + " " + strings.Repeat("b", 10),
			// first 60 runes are 59 'a's + the space; the final .strip()
			// after truncation removes that trailing space.
			want: strings.Repeat("a", 59),
		},
		{
			name:  "tabs and newlines are control chars, stripped not collapsed",
			input: "\t\nHello World\n\t",
			want:  "Hello World",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Sanitize(tt.input); got != tt.want {
				t.Errorf("Sanitize(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSanitizeN(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{
			name:   "custom max_len truncates like python max_len=5",
			input:  "Hello World Extra",
			maxLen: 5,
			want:   "Hello",
		},
		{
			name:   "custom max_len 5 with no internal dot to strip",
			input:  "Hello.",
			maxLen: 5,
			want:   "Hello",
		},
		{
			name:   "only dots at default max_len falls back to untitled",
			input:  "....",
			maxLen: DefaultMaxLen,
			want:   "untitled",
		},
		{
			name:   "valid name passes through unchanged",
			input:  "valid_name-123",
			maxLen: DefaultMaxLen,
			want:   "valid_name-123",
		},
		{
			name:   "negative maxLen clamps to 0 and falls back to untitled",
			input:  "Hello World",
			maxLen: -3,
			want:   "untitled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeN(tt.input, tt.maxLen); got != tt.want {
				t.Errorf("SanitizeN(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestNewBOMTolerantReader(t *testing.T) {
	t.Run("without BOM", func(t *testing.T) {
		r := NewBOMTolerantReader(strings.NewReader("url,title\nhttps://x,Chat One\n"))
		records, err := r.ReadAll()
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		want := [][]string{
			{"url", "title"},
			{"https://x", "Chat One"},
		}
		assertRecordsEqual(t, records, want)
	})

	t.Run("with UTF-8 BOM", func(t *testing.T) {
		input := "\xEF\xBB\xBFurl,title\nhttps://x,Chat One\n"
		r := NewBOMTolerantReader(strings.NewReader(input))
		records, err := r.ReadAll()
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		want := [][]string{
			{"url", "title"},
			{"https://x", "Chat One"},
		}
		assertRecordsEqual(t, records, want)
		// Specifically confirm the BOM didn't get glued onto the first
		// header cell, which is the actual failure mode this guards
		// against (a header of "<BOM>url" would break lookups for "url").
		if records[0][0] != "url" {
			t.Errorf("first field = %q, want %q (BOM not stripped)", records[0][0], "url")
		}
	})

	t.Run("ragged rows are tolerated like Python's csv.reader", func(t *testing.T) {
		// export_chats.py relies on ragged rows being readable, e.g.
		// `row[1].strip() if len(row) > 1 else ""`.
		input := "url,title\nhttps://x/c/1\nhttps://x/c/2,Two,extra\n"
		r := NewBOMTolerantReader(strings.NewReader(input))
		records, err := r.ReadAll()
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		want := [][]string{
			{"url", "title"},
			{"https://x/c/1"},
			{"https://x/c/2", "Two", "extra"},
		}
		assertRecordsEqual(t, records, want)
	})

	t.Run("empty input with no BOM", func(t *testing.T) {
		r := NewBOMTolerantReader(strings.NewReader(""))
		records, err := r.ReadAll()
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		if len(records) != 0 {
			t.Errorf("records = %v, want empty", records)
		}
	})
}

func assertRecordsEqual(t *testing.T, got, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("record %d: got %v, want %v", i, got[i], want[i])
		}
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Errorf("record %d field %d: got %q, want %q", i, j, got[i][j], want[i][j])
			}
		}
	}
}
