// Package policy implements PolicyScript (docs/POLICY_LANGUAGE.md): a
// hand-written lexer, a recursive-descent parser, a static type checker and a
// tree-walking interpreter. A Program is immutable after Compile and safe for
// concurrent use.
package policy

import (
	"fmt"
	"strconv"
	"strings"
)

// Hard limits (docs/POLICY_LANGUAGE.md §6).
const (
	MaxSource    = 64 << 10
	MaxNodes     = 10_000
	MaxDepth     = 64
	MaxStringLit = 4 << 10
	MaxListLen   = 256
	maxInt       = 1 << 53
)

type tokKind uint8

const (
	tEOF tokKind = iota
	tIdent
	tString
	tInt
	tPunct // { } ( ) [ ] , . ; + ! && || == != < <= > >=
)

type pos struct{ line, col int }

func (p pos) String() string { return fmt.Sprintf("%d:%d", p.line, p.col) }

type token struct {
	kind tokKind
	text string // identifier, punctuation, or the decoded string literal
	num  int64
	pos  pos
}

// Error is a load-time error at a source position.
type Error struct {
	Line, Col int
	Msg       string
}

func (e *Error) Error() string { return fmt.Sprintf("%d:%d: %s", e.Line, e.Col, e.Msg) }

// ErrorList is every type error found in one program.
type ErrorList []*Error

func (l ErrorList) Error() string {
	s := make([]string, len(l))
	for i, e := range l {
		s[i] = e.Error()
	}
	return strings.Join(s, "\n")
}

func errAt(p pos, format string, a ...any) *Error {
	return &Error{Line: p.line, Col: p.col, Msg: fmt.Sprintf(format, a...)}
}

type lexer struct {
	src       string
	i         int
	line, col int
}

// lex turns src into tokens, ending with tEOF.
func lex(src string) ([]token, error) {
	if len(src) > MaxSource {
		return nil, errAt(pos{1, 1}, "policy is %d bytes, limit is %d", len(src), MaxSource)
	}
	lx := &lexer{src: src, line: 1, col: 1}
	var toks []token
	for {
		t, err := lx.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, t)
		if t.kind == tEOF {
			return toks, nil
		}
	}
}

func (lx *lexer) advance() byte {
	c := lx.src[lx.i]
	lx.i++
	if c == '\n' {
		lx.line, lx.col = lx.line+1, 1
	} else {
		lx.col++
	}
	return c
}

func (lx *lexer) peek(off int) byte {
	if lx.i+off < len(lx.src) {
		return lx.src[lx.i+off]
	}
	return 0
}

func (lx *lexer) next() (token, error) {
	// Skip whitespace and comments.
	for lx.i < len(lx.src) {
		c := lx.src[lx.i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			lx.advance()
		case c == '#' || (c == '/' && lx.peek(1) == '/'):
			for lx.i < len(lx.src) && lx.src[lx.i] != '\n' {
				lx.advance()
			}
		default:
			goto tok
		}
	}
tok:
	p := pos{lx.line, lx.col}
	if lx.i >= len(lx.src) {
		return token{kind: tEOF, pos: p}, nil
	}
	c := lx.src[lx.i]
	switch {
	case isLetter(c):
		start := lx.i
		for lx.i < len(lx.src) && (isLetter(lx.src[lx.i]) || isDigit(lx.src[lx.i])) {
			lx.advance()
		}
		return token{kind: tIdent, text: lx.src[start:lx.i], pos: p}, nil
	case isDigit(c):
		start := lx.i
		for lx.i < len(lx.src) && isDigit(lx.src[lx.i]) {
			lx.advance()
		}
		n, err := strconv.ParseInt(lx.src[start:lx.i], 10, 64)
		if err != nil || n > maxInt {
			return token{}, errAt(p, "integer %s is too large", lx.src[start:lx.i])
		}
		if lx.i < len(lx.src) && isLetter(lx.src[lx.i]) {
			return token{}, errAt(p, "invalid number %s%c", lx.src[start:lx.i], lx.src[lx.i])
		}
		return token{kind: tInt, num: n, pos: p}, nil
	case c == '"':
		return lx.str(p)
	}
	two := ""
	if lx.i+1 < len(lx.src) {
		two = lx.src[lx.i : lx.i+2]
	}
	switch two {
	case "&&", "||", "==", "!=", "<=", ">=":
		lx.advance()
		lx.advance()
		return token{kind: tPunct, text: two, pos: p}, nil
	}
	if strings.IndexByte("{}()[],.;+!<>", c) >= 0 {
		lx.advance()
		return token{kind: tPunct, text: string(c), pos: p}, nil
	}
	if c == '&' || c == '|' || c == '=' {
		return token{}, errAt(p, "unexpected %q (did you mean %q?)", c, string([]byte{c, c}))
	}
	return token{}, errAt(p, "unexpected character %q", rune(c))
}

func (lx *lexer) str(p pos) (token, error) {
	lx.advance() // opening quote
	var b strings.Builder
	for {
		if lx.i >= len(lx.src) {
			return token{}, errAt(p, "unterminated string")
		}
		c := lx.advance()
		switch c {
		case '"':
			if b.Len() > MaxStringLit {
				return token{}, errAt(p, "string literal is %d bytes, limit is %d", b.Len(), MaxStringLit)
			}
			return token{kind: tString, text: b.String(), pos: p}, nil
		case '\n':
			return token{}, errAt(p, "newline in string")
		case '\\':
			if lx.i >= len(lx.src) {
				return token{}, errAt(p, "unterminated string")
			}
			ep := pos{lx.line, lx.col - 1}
			switch e := lx.advance(); e {
			case '"', '\\':
				b.WriteByte(e)
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				return token{}, errAt(ep, "unknown escape \\%c", e)
			}
		default:
			b.WriteByte(c)
		}
	}
}

func isLetter(c byte) bool { return c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z') }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
