package nginx

import (
	"errors"
	"strings"
)

type tokenKind int

const (
	tokWord tokenKind = iota
	tokSemi
	tokOpen
	tokClose
)

type token struct {
	kind tokenKind
	text string
}

// tokenize splits an nginx configuration into words, ';', '{' and '}'.
// Comments run to the end of the line; single and double quotes group
// a word and support backslash escapes.
func tokenize(src string) []token {
	var toks []token
	var cur strings.Builder
	quoted := false // the current word came from a quoted string

	flush := func() {
		if cur.Len() > 0 || quoted {
			toks = append(toks, token{tokWord, cur.String()})
			cur.Reset()
			quoted = false
		}
	}

	for i := 0; i < len(src); i++ {
		ch := src[i]
		switch ch {
		case '#':
			flush()
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case '"', '\'':
			quote := ch
			quoted = true
			i++
			for i < len(src) && src[i] != quote {
				if src[i] == '\\' && i+1 < len(src) {
					i++
				}
				cur.WriteByte(src[i])
				i++
			}
		case ' ', '\t', '\r', '\n':
			flush()
		case ';':
			flush()
			toks = append(toks, token{tokSemi, ";"})
		case '{':
			flush()
			toks = append(toks, token{tokOpen, "{"})
		case '}':
			flush()
			toks = append(toks, token{tokClose, "}"})
		default:
			cur.WriteByte(ch)
		}
	}
	flush()
	return toks
}

var errUnexpectedEOF = errors.New("unexpected end of file")

// parseBlock consumes tokens until the end of the input (nested =
// false) or the matching '}' (nested = true), returning the directives
// found and the tokens left over.
func parseBlock(toks []token, nested bool) ([]directive, []token, error) {
	var out []directive
	var words []string

	for len(toks) > 0 {
		t := toks[0]
		toks = toks[1:]
		switch t.kind {
		case tokWord:
			words = append(words, t.text)
		case tokSemi:
			if len(words) > 0 {
				out = append(out, directive{name: words[0], args: words[1:]})
				words = nil
			}
		case tokOpen:
			if len(words) == 0 {
				return nil, nil, errors.New("block without a directive name")
			}
			block, rest, err := parseBlock(toks, true)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, directive{name: words[0], args: words[1:], block: block})
			words, toks = nil, rest
		case tokClose:
			if !nested {
				return out, append([]token{t}, toks...), nil
			}
			return out, toks, nil
		}
	}
	if nested {
		return nil, nil, errUnexpectedEOF
	}
	return out, nil, nil
}
