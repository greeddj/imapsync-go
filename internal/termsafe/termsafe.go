// Package termsafe makes text from IMAP servers safe to print on a UTF-8 terminal.
// A terminal that acts on 8-bit C1 bytes inside multi-byte runes is out of scope.
package termsafe

import (
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const lowerHex = "0123456789abcdef"

// Escape replaces non-graphic runes, invisible graphic runes and invalid UTF-8 bytes in s
// with Go-style escapes.
func Escape(s string) string {
	i := firstUnsafe(s)
	if i < 0 {
		return s
	}

	var b strings.Builder
	b.Grow(len(s) + 16)
	b.WriteString(s[:i])
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if needsEscape(r, size) {
			writeEscape(&b, r, s[i])
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// strconv.IsGraphic accepts invisible runes such as U+3164, so Other_Default_Ignorable
// runes are escaped too. Otherwise two folder names could look the same.
func needsEscape(r rune, size int) bool {
	if r < utf8.RuneSelf {
		return r < ' ' || r == 0x7f
	}
	// DecodeRuneInString returns U+FFFD with size 1 for a byte that is not UTF-8.
	if r == utf8.RuneError && size == 1 {
		return true
	}
	return !strconv.IsGraphic(r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r)
}

func firstUnsafe(s string) int {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if needsEscape(r, size) {
			return i
		}
		i += size
	}
	return -1
}

// A RuneError here stands for the one invalid byte raw: a valid U+FFFD is graphic
// and never gets this far.
func writeEscape(b *strings.Builder, r rune, raw byte) {
	switch r {
	case utf8.RuneError:
		writeHex(b, 'x', uint32(raw), 2)
	case '\a':
		b.WriteString(`\a`)
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
	case '\v':
		b.WriteString(`\v`)
	default:
		switch {
		case r < utf8.RuneSelf:
			writeHex(b, 'x', uint32(r), 2)
		case r < 0x10000:
			writeHex(b, 'u', uint32(r), 4)
		default:
			writeHex(b, 'U', uint32(r), 8)
		}
	}
}

func writeHex(b *strings.Builder, kind byte, v uint32, digits int) {
	b.WriteByte('\\')
	b.WriteByte(kind)
	for shift := 4 * (digits - 1); shift >= 0; shift -= 4 {
		b.WriteByte(lowerHex[(v>>shift)&0xf])
	}
}

// Writer escapes everything written through it before passing it on.
type Writer struct{ w io.Writer }

// NewWriter returns a Writer that forwards escaped output to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write escapes p and forwards it in one call, keeping one trailing newline raw.
func (w *Writer) Write(p []byte) (int, error) {
	body, newline := p, false
	if n := len(p); n > 0 && p[n-1] == '\n' {
		body, newline = p[:n-1], true
	}

	esc := Escape(string(body))
	out := make([]byte, 0, len(esc)+1)
	out = append(out, esc...)
	if newline {
		out = append(out, '\n')
	}
	if _, err := w.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}
