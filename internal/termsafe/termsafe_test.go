package termsafe

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

type escapeCase struct {
	name string
	in   string
	want string
}

func escapeCases() []escapeCase {
	cases := []escapeCase{
		{name: "ASCII name is unchanged", in: "INBOX", want: "INBOX"},
		{name: "Cyrillic path is unchanged", in: "Входящие/Отправленные", want: "Входящие/Отправленные"},
		{name: "CJK and emoji are unchanged", in: "日本語 📁", want: "日本語 📁"},
		{name: "no-break space is unchanged", in: "a\u00a0b", want: "a\u00a0b"},
		{name: "emoji with a variation selector is unchanged", in: "\u2764\ufe0f", want: "\u2764\ufe0f"},
		{name: "Korean name is unchanged", in: "\ubc1b\uc740\ud3b8\uc9c0\ud568", want: "\ubc1b\uc740\ud3b8\uc9c0\ud568"},
		{name: "valid replacement character is unchanged", in: "\ufffd", want: "\ufffd"},
		{name: "backslash is unchanged", in: `a\b`, want: `a\b`},
		{name: "quotes are unchanged", in: `say "hi" 'x'`, want: `say "hi" 'x'`},
		{name: "empty string stays empty", in: "", want: ""},

		{name: "CSI sequence", in: "\x1b[2J", want: `\x1b[2J`},
		{name: "OSC sequence with bell", in: "\x1b]0;pwned\x07", want: `\x1b]0;pwned\a`},
		{name: "CR LF and TAB", in: "a\rb\nc\td", want: `a\rb\nc\td`},
		{name: "every short escape", in: "\a\b\f\v", want: `\a\b\f\v`},
		{name: "NUL", in: "\x00", want: `\x00`},
		{name: "DEL", in: "\x7f", want: `\x7f`},
		{name: "C1 next line", in: "\u0085", want: `\u0085`},
		{name: "C1 single shift CSI", in: "\u009b", want: `\u009b`},

		{name: "left-to-right mark", in: "\u200e", want: `\u200e`},
		{name: "right-to-left mark", in: "\u200f", want: `\u200f`},
		{name: "arabic letter mark", in: "\u061c", want: `\u061c`},
		{name: "line separator", in: "\u2028", want: `\u2028`},
		{name: "paragraph separator", in: "\u2029", want: `\u2029`},
		{name: "zero width space", in: "\u200b", want: `\u200b`},
		{name: "zero width joiner", in: "\u200d", want: `\u200d`},
		{name: "byte order mark", in: "\ufeff", want: `\ufeff`},
		{name: "soft hyphen", in: "\u00ad", want: `\u00ad`},
		{name: "private use", in: "\ue000", want: `\ue000`},
		{name: "unassigned", in: "\u0378", want: `\u0378`},
		{name: "tag character", in: "\U000E0041", want: `\U000e0041`},
		{name: "noncharacter above the BMP", in: "\U0010ffff", want: `\U0010ffff`},
		{name: "combining grapheme joiner", in: "\u034f", want: `\u034f`},
		{name: "hangul choseong filler", in: "\u115f", want: `\u115f`},
		{name: "hangul jungseong filler", in: "\u1160", want: `\u1160`},
		{name: "khmer vowel inherent AQ", in: "\u17b4", want: `\u17b4`},
		{name: "khmer vowel inherent AA", in: "\u17b5", want: `\u17b5`},
		{name: "hangul filler", in: "\u3164", want: `\u3164`},
		{name: "halfwidth hangul filler", in: "\uffa0", want: `\uffa0`},
		{name: "hangul filler inside a name", in: "Inbox\u3164", want: `Inbox\u3164`},

		{name: "invalid byte", in: "\xff", want: `\xff`},
		{name: "truncated sequence", in: "\xd0", want: `\xd0`},
		{name: "stray continuation byte", in: "\x9b", want: `\x9b`},
		{name: "encoded surrogate", in: "\xed\xa0\x80", want: `\xed\xa0\x80`},
		{name: "invalid byte inside text", in: "ab\xffcd", want: `ab\xffcd`},

		{name: "Cyrillic keeps its letters next to an escape", in: "Папка\x1b[2Jесть", want: `Папка\x1b[2Jесть`},
		{name: "hostile folder name", in: "Evil\x1b[2J\rx\u202egpj", want: `Evil\x1b[2J\rx\u202egpj`},
		{name: "escape at the start and the end", in: "\x1bok\x1b", want: `\x1bok\x1b`},
	}
	for r := rune(0x202a); r <= 0x202e; r++ {
		cases = append(cases, escapeCase{
			name: fmt.Sprintf("bidi embedding U+%04X", r),
			in:   string(r),
			want: `\u` + strconv.FormatInt(int64(r), 16),
		})
	}
	for r := rune(0x2066); r <= 0x2069; r++ {
		cases = append(cases, escapeCase{
			name: fmt.Sprintf("bidi isolate U+%04X", r),
			in:   string(r),
			want: `\u` + strconv.FormatInt(int64(r), 16),
		})
	}
	return cases
}

func allVisible(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !strconv.IsGraphic(r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) {
			return false
		}
	}
	return true
}

func TestEscape(t *testing.T) {
	t.Parallel()

	for _, tt := range escapeCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Escape(tt.in)

			if got != tt.want {
				t.Errorf("Escape(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !allVisible(got) {
				t.Errorf("Escape(%q) = %q, which still holds an invisible rune", tt.in, got)
			}
		})
	}
}

func TestEscape_When_InputIsOneASCIIRune_Then_OnlyC0AndDELChange(t *testing.T) {
	t.Parallel()

	for r := rune(0); r < utf8.RuneSelf; r++ {
		in := string(r)
		control := r < 0x20 || r == 0x7f

		got := Escape(in)

		if unchanged := got == in; unchanged == control {
			t.Errorf("Escape(%q) = %q, control rune = %v", in, got, control)
		}
	}
}

// Not parallel: AllocsPerRun counts allocations of the whole process.
func TestEscape_When_NothingNeedsEscaping_Then_ItDoesNotAllocate(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "ASCII", in: "Archive/2025/Invoices"},
		{name: "Cyrillic", in: "Входящие/Отправленные"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			allocs := testing.AllocsPerRun(100, func() { got = Escape(tt.in) })

			if allocs != 0 {
				t.Errorf("allocations per call = %v, want 0", allocs)
			}
			if got != tt.in {
				t.Errorf("Escape(%q) = %q, want the input back", tt.in, got)
			}
		})
	}
}

func FuzzEscape(f *testing.F) {
	for _, tt := range escapeCases() {
		f.Add(tt.in)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := Escape(in)

		if !allVisible(got) {
			t.Errorf("Escape(%q) = %q, which holds an invisible rune or invalid UTF-8", in, got)
		}
		if allVisible(in) && got != in {
			t.Errorf("Escape(%q) = %q, want the visible input back", in, got)
		}
		if again := Escape(got); again != got {
			t.Errorf("Escape(Escape(%q)) = %q, want %q", in, again, got)
		}
	})
}

func TestWriter_When_LoggerWritesALine_Then_OneRawNewlineEndsTheEscapedLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	l := log.New(NewWriter(&buf), "imap/client: ", 0)

	l.Println("cannot handle response ", "\x1b[2J\r\n\u202eX")

	want := `imap/client: cannot handle response  \x1b[2J\r\n\u202eX` + "\n"
	if got := buf.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Errorf("output holds %d raw newlines, want 1", n)
	}
}

func TestWriter_Write(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "no trailing newline gets none", in: "a\x1b[2J", want: `a\x1b[2J`},
		{name: "one trailing newline stays raw", in: "a\x1b\n", want: `a\x1b` + "\n"},
		{name: "only the last of two newlines stays raw", in: "a\n\n", want: `a\n` + "\n"},
		{name: "a lone newline stays raw", in: "\n", want: "\n"},
		{name: "empty write stays empty", in: "", want: ""},
		{name: "graphic text passes through", in: "plain text\n", want: "plain text\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			n, err := NewWriter(&buf).Write([]byte(tt.in))

			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if n != len(tt.in) {
				t.Errorf("Write returned %d, want len(p) = %d", n, len(tt.in))
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
		})
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 3, w.err }

func TestWriter_When_UnderlyingWriteFails_Then_ReturnsZeroAndTheError(t *testing.T) {
	t.Parallel()

	boom := errors.New("disk full")

	n, err := NewWriter(failingWriter{err: boom}).Write([]byte("x\x1b\n"))

	if n != 0 {
		t.Errorf("Write returned n = %d, want 0", n)
	}
	if !errors.Is(err, boom) {
		t.Errorf("Write error = %v, want %v", err, boom)
	}
}
