// Package ignore implements .dockerignore matching.
//
// Semantics follow Docker: one pattern per line, '#' starts a comment,
// '!' re-includes, '**' matches any number of directories, and the last
// matching pattern wins. A pattern that matches a directory also matches
// everything below it.
package ignore

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

type pattern struct {
	raw     string
	re      *regexp.Regexp
	exclude bool // a "!" pattern
}

// Matcher reports whether context paths are excluded.
type Matcher struct {
	patterns   []pattern
	exceptions bool
}

// Parse reads .dockerignore patterns.
func Parse(r io.Reader) (*Matcher, error) {
	m := &Matcher{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		exclude := false
		if strings.HasPrefix(line, "!") {
			exclude = true
			line = strings.TrimSpace(line[1:])
		}
		line = strings.TrimPrefix(path.Clean("/"+line), "/")
		if line == "" {
			continue // "." or "/" would exclude the whole context; Docker ignores it
		}
		re, err := compile(line)
		if err != nil {
			return nil, fmt.Errorf("invalid .dockerignore pattern %q: %w", line, err)
		}
		m.patterns = append(m.patterns, pattern{raw: line, re: re, exclude: exclude})
		if exclude {
			m.exceptions = true
		}
	}
	return m, sc.Err()
}

// compile converts a pattern to a regular expression the way Docker's
// patternmatcher does.
func compile(p string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	rs := []rune(p)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '*':
			if i+1 < len(rs) && rs[i+1] == '*' {
				i++
				if i+1 < len(rs) && rs[i+1] == '/' {
					i++
				}
				if i+1 >= len(rs) {
					b.WriteString(".*")
				} else {
					b.WriteString("(.*/)?")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case c == '?':
			b.WriteString("[^/]")
		case c == '\\':
			if i+1 < len(rs) {
				i++
				b.WriteString(regexp.QuoteMeta(string(rs[i])))
			} else {
				b.WriteString(`\\`)
			}
		case c == '[':
			// Pass character classes through; "[!x]" means "[^x]".
			end := strings.IndexRune(string(rs[i:]), ']')
			if end < 0 {
				return nil, fmt.Errorf("unterminated character class")
			}
			class := string(rs[i+1 : i+end])
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += end
		case strings.ContainsRune(".$^+(){}|", c):
			b.WriteString(`\` + string(c))
		default:
			b.WriteRune(c)
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// Excluded reports whether rel (slash-separated, relative to the context
// root) is excluded.
func (m *Matcher) Excluded(rel string) bool {
	if m == nil || rel == "" || rel == "." {
		return false
	}
	matched := false
	for _, p := range m.patterns {
		// An exclusion only matters if already matched, and vice versa.
		if p.exclude != matched {
			continue
		}
		if p.matches(rel) {
			matched = !p.exclude
		}
	}
	return matched
}

// HasExceptions reports whether there are "!" patterns, in which case an
// excluded directory may still contain included files.
func (m *Matcher) HasExceptions() bool { return m != nil && m.exceptions }

func (p pattern) matches(rel string) bool {
	if p.re.MatchString(rel) {
		return true
	}
	// A pattern matching a parent directory matches the path too.
	for dir := path.Dir(rel); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if p.re.MatchString(dir) {
			return true
		}
	}
	return false
}
