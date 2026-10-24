package dockerfile

import (
	"fmt"
	"strings"
)

// Lookup returns the value of a build variable and whether it is set.
type Lookup func(name string) (string, bool)

// ProcessWord expands variables in a Dockerfile word and removes quoting,
// following the rules Docker uses for ADD, COPY, ENV, EXPOSE, FROM, LABEL,
// STOPSIGNAL, USER, VOLUME and WORKDIR:
//
//   - $name and ${name} expand to the value, or "" if unset.
//   - ${name:-word} / ${name-word}: word if name is unset or empty / unset.
//   - ${name:+word} / ${name+word}: word if name is set and non-empty / set.
//   - ${name:?msg} / ${name?msg}: error if name is unset or empty / unset.
//   - Single quotes are literal; double quotes allow expansion.
//   - The escape character makes the next character literal.
func ProcessWord(word string, lookup Lookup, esc rune) (string, error) {
	p := &wordProcessor{src: []rune(word), lookup: lookup, esc: esc, quotes: true}
	return p.process(0)
}

// ExpandVars expands variables like ProcessWord but leaves quotes alone.
// It is used for exec-form (JSON) arguments, which are already unquoted.
func ExpandVars(word string, lookup Lookup, esc rune) (string, error) {
	p := &wordProcessor{src: []rune(word), lookup: lookup, esc: esc}
	return p.process(0)
}

type wordProcessor struct {
	src    []rune
	pos    int
	lookup Lookup
	esc    rune
	quotes bool
}

// process consumes runes until the end of input, or until an unmatched
// '}' when depth > 0 (the word of a ${name:-word} modifier).
func (p *wordProcessor) process(depth int) (string, error) {
	var out strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == '}' && depth > 0:
			return out.String(), nil
		case c == p.esc:
			p.pos++
			if p.pos >= len(p.src) {
				out.WriteRune(c)
				break
			}
			n := p.src[p.pos]
			if !p.quotes && n != '$' {
				out.WriteRune(c) // outside of shell quoting only "\$" is an escape
			}
			out.WriteRune(n)
			p.pos++
		case c == '\'' && p.quotes:
			p.pos++
			end := p.indexFrom('\'')
			if end < 0 {
				return "", fmt.Errorf("unterminated single quote in %q", string(p.src))
			}
			out.WriteString(string(p.src[p.pos:end]))
			p.pos = end + 1
		case c == '"' && p.quotes:
			p.pos++
			s, err := p.doubleQuoted()
			if err != nil {
				return "", err
			}
			out.WriteString(s)
		case c == '$':
			s, err := p.variable()
			if err != nil {
				return "", err
			}
			out.WriteString(s)
		default:
			out.WriteRune(c)
			p.pos++
		}
	}
	if depth > 0 {
		return "", fmt.Errorf("missing '}' in %q", string(p.src))
	}
	return out.String(), nil
}

func (p *wordProcessor) indexFrom(r rune) int {
	for i := p.pos; i < len(p.src); i++ {
		if p.src[i] == r {
			return i
		}
	}
	return -1
}

func (p *wordProcessor) doubleQuoted() (string, error) {
	var out strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == '"':
			p.pos++
			return out.String(), nil
		case c == p.esc && p.pos+1 < len(p.src):
			n := p.src[p.pos+1]
			if n == '"' || n == '$' || n == p.esc {
				out.WriteRune(n)
			} else {
				out.WriteRune(c)
				out.WriteRune(n)
			}
			p.pos += 2
		case c == '$':
			s, err := p.variable()
			if err != nil {
				return "", err
			}
			out.WriteString(s)
		default:
			out.WriteRune(c)
			p.pos++
		}
	}
	return "", fmt.Errorf("unterminated double quote in %q", string(p.src))
}

func isNameRune(r rune, first bool) bool {
	switch {
	case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return !first
	}
	return false
}

// variable expands a $ reference at p.pos.
func (p *wordProcessor) variable() (string, error) {
	p.pos++ // '$'
	if p.pos >= len(p.src) {
		return "$", nil
	}
	if p.src[p.pos] != '{' {
		start := p.pos
		for p.pos < len(p.src) && isNameRune(p.src[p.pos], p.pos == start) {
			p.pos++
		}
		if p.pos == start {
			return "$", nil // not a variable reference
		}
		v, _ := p.lookup(string(p.src[start:p.pos]))
		return v, nil
	}

	p.pos++ // '{'
	start := p.pos
	for p.pos < len(p.src) && isNameRune(p.src[p.pos], p.pos == start) {
		p.pos++
	}
	name := string(p.src[start:p.pos])
	if name == "" {
		return "", fmt.Errorf("bad substitution in %q", string(p.src))
	}
	if p.pos >= len(p.src) {
		return "", fmt.Errorf("missing '}' in %q", string(p.src))
	}
	val, set := p.lookup(name)
	if p.src[p.pos] == '}' {
		p.pos++
		return val, nil
	}

	colon := false
	if p.src[p.pos] == ':' {
		colon = true
		p.pos++
		if p.pos >= len(p.src) {
			return "", fmt.Errorf("missing '}' in %q", string(p.src))
		}
	}
	op := p.src[p.pos]
	if op != '-' && op != '+' && op != '?' {
		return "", fmt.Errorf("unsupported modifier %q in %q", string(op), string(p.src))
	}
	p.pos++
	word, err := p.process(1)
	if err != nil {
		return "", err
	}
	p.pos++ // '}'

	present := set && (!colon || val != "")
	switch op {
	case '-':
		if present {
			return val, nil
		}
		return word, nil
	case '+':
		if present {
			return word, nil
		}
		return "", nil
	default: // '?'
		if present {
			return val, nil
		}
		if word == "" {
			word = "is not set"
		}
		return "", fmt.Errorf("%s: %s", name, word)
	}
}
