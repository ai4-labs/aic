// Package dockerfile parses Dockerfiles into a list of instructions.
//
// The parser handles the syntax only: parser directives, comments, line
// continuations, exec (JSON) vs shell form, leading --flags, and heredocs.
// Variable substitution is done later by the builder with ProcessWord,
// because its inputs (ARG and ENV values) are only known while building.
package dockerfile

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// MaxSize bounds the size of a Dockerfile that Parse accepts.
const MaxSize = 10 << 20

// Heredoc is an inline document introduced with <<NAME.
type Heredoc struct {
	Name    string
	Content string
	// Expand is false when the delimiter was quoted (<<"EOF" or <<'EOF'),
	// which disables variable expansion in the content.
	Expand bool
}

// Instruction is one Dockerfile instruction.
type Instruction struct {
	// Cmd is the lower-case keyword, e.g. "run".
	Cmd string
	// Flags are the leading --name[=value] tokens, verbatim.
	Flags []string
	// Args depends on Cmd:
	//   - RUN, CMD, ENTRYPOINT: the exec-form argv if JSON, else one shell string.
	//   - ENV, LABEL: alternating raw key, raw value (values still quoted).
	//   - ARG: raw "name" or "name=default" tokens.
	//   - HEALTHCHECK: ["NONE"], or "CMD" followed by argv (JSON) or one shell string.
	//   - Others: whitespace-separated words, or one string where the
	//     instruction takes free text (WORKDIR, USER, MAINTAINER, ...).
	Args []string
	// JSON reports whether the exec (JSON array) form was used.
	JSON     bool
	Heredocs []Heredoc
	// Original is the logical line, after joining continuations.
	Original string
	// Line is the 1-based line number where the instruction starts.
	Line int
}

// Flag returns the value of the flag --name and whether it was present.
// A flag without "=" has the value "true".
func (ins *Instruction) Flag(name string) (string, bool) {
	for _, f := range ins.Flags {
		k, v, hasEq := strings.Cut(strings.TrimPrefix(f, "--"), "=")
		if k == name {
			if !hasEq {
				v = "true"
			}
			return v, true
		}
	}
	return "", false
}

// Dockerfile is a parsed Dockerfile.
type Dockerfile struct {
	// Escape is the escape character: '\\' (default) or '`'.
	Escape       rune
	Instructions []Instruction
}

// Error is a parse error with a line number.
type Error struct {
	Line int
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

var (
	directiveRE = regexp.MustCompile(`^#\s*([a-zA-Z][a-zA-Z0-9]*)\s*=\s*(.+?)\s*$`)
	heredocRE   = regexp.MustCompile(`^<<(-?)(?:"([A-Za-z_][A-Za-z0-9_]*)"|'([A-Za-z_][A-Za-z0-9_]*)'|([A-Za-z_][A-Za-z0-9_]*))`)
)

var knownCmds = map[string]bool{
	"add": true, "arg": true, "cmd": true, "copy": true, "entrypoint": true,
	"env": true, "expose": true, "from": true, "healthcheck": true, "label": true,
	"maintainer": true, "onbuild": true, "run": true, "shell": true,
	"stopsignal": true, "user": true, "volume": true, "workdir": true,
}

// Instructions whose leading --flags are parsed.
var flagCmds = map[string]bool{"from": true, "run": true, "copy": true, "add": true, "healthcheck": true}

// Parse parses a Dockerfile.
func Parse(r io.Reader) (*Dockerfile, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSize {
		return nil, fmt.Errorf("dockerfile larger than %d bytes", MaxSize)
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")

	df := &Dockerfile{Escape: '\\'}
	i := 0

	// Parser directives are only recognized at the very top.
	seen := map[string]bool{}
	for ; i < len(lines); i++ {
		m := directiveRE.FindStringSubmatch(lines[i])
		if m == nil {
			break
		}
		key := strings.ToLower(m[1])
		if key != "escape" && key != "syntax" && key != "check" {
			break // an unknown directive is a comment and ends the directive block
		}
		if seen[key] {
			return nil, &Error{i + 1, fmt.Sprintf("only one %s directive is allowed", key)}
		}
		seen[key] = true
		if key == "escape" {
			if m[2] != `\` && m[2] != "`" {
				return nil, &Error{i + 1, fmt.Sprintf("invalid escape character %q: must be \\ or `", m[2])}
			}
			df.Escape = rune(m[2][0])
		}
		// syntax= and check= only matter to BuildKit frontends; ignore them.
	}

	for i < len(lines) {
		lineNo := i + 1
		logical := lines[i]
		i++
		if blankOrComment(logical) {
			continue
		}
		for {
			body, cont := trimContinuation(logical, df.Escape)
			logical = body
			if !cont {
				break
			}
			found := false
			for i < len(lines) {
				next := lines[i]
				i++
				if blankOrComment(next) {
					continue // comments and empty lines inside a continuation are dropped
				}
				logical += next
				found = true
				break
			}
			if !found {
				break
			}
		}
		ins, err := parseLine(strings.TrimSpace(logical), lineNo, df.Escape)
		if err != nil {
			return nil, err
		}
		if (ins.Cmd == "run" || ins.Cmd == "copy" || ins.Cmd == "add") && !ins.JSON {
			if i, err = readHeredocs(&ins, lines, i); err != nil {
				return nil, err
			}
		}
		df.Instructions = append(df.Instructions, ins)
	}
	return df, nil
}

func blankOrComment(s string) bool {
	t := strings.TrimSpace(s)
	return t == "" || strings.HasPrefix(t, "#")
}

func trimContinuation(s string, esc rune) (string, bool) {
	t := strings.TrimRight(s, " \t")
	if strings.HasSuffix(t, string(esc)) {
		return t[:len(t)-1], true
	}
	return t, false
}

// readHeredocs consumes the bodies of any heredocs started on ins's line.
// It returns the index of the first line after the last terminator.
func readHeredocs(ins *Instruction, lines []string, i int) (int, error) {
	if len(ins.Args) == 0 {
		return i, nil
	}
	src := ins.Args[0]
	var matches [][]string
	if ins.Cmd == "run" {
		// As in BuildKit, a heredoc starts a shell word, possibly after a
		// file descriptor: "$((1<<n))" and "<<" in quotes don't.
		for _, w := range splitWords(src, '\\') {
			if m := heredocRE.FindStringSubmatch(strings.TrimLeft(w, "0123456789")); m != nil {
				matches = append(matches, m)
			}
		}
	} else {
		// COPY and ADD sources that are a heredoc and nothing else.
		src = strings.Join(ins.Args, " ")
		for _, a := range ins.Args[:len(ins.Args)-1] {
			if m := heredocRE.FindStringSubmatch(a); m != nil && m[0] == a {
				matches = append(matches, m)
			}
		}
	}
	if len(matches) == 0 {
		return i, nil
	}
	var script strings.Builder
	script.WriteString(src)
	for _, m := range matches {
		strip := m[1] == "-"
		name := m[2] + m[3] + m[4]
		var body []string
		var raw []string
		terminated := false
		for i < len(lines) {
			l := lines[i]
			i++
			raw = append(raw, l)
			cmp := l
			if strip {
				cmp = strings.TrimLeft(l, "\t")
			}
			if cmp == name {
				terminated = true
				break
			}
			if strip {
				l = strings.TrimLeft(l, "\t")
			}
			body = append(body, l)
		}
		if !terminated {
			return i, &Error{ins.Line, fmt.Sprintf("unterminated heredoc %q", name)}
		}
		content := ""
		if len(body) > 0 {
			content = strings.Join(body, "\n") + "\n"
		}
		ins.Heredocs = append(ins.Heredocs, Heredoc{Name: name, Content: content, Expand: m[4] != ""})
		script.WriteString("\n")
		script.WriteString(strings.Join(raw, "\n"))
	}
	if ins.Cmd == "run" {
		// "RUN <<EOF" on its own runs the document as a script. Otherwise
		// the shell gets the command line followed by the heredoc bodies,
		// exactly as it would in a script file.
		if len(matches) == 1 && strings.TrimSpace(src) == matches[0][0] {
			ins.Args = []string{ins.Heredocs[0].Content}
		} else {
			ins.Args = []string{script.String()}
		}
	}
	return i, nil
}

func parseLine(line string, lineNo int, esc rune) (Instruction, error) {
	var kw, rest string
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		kw, rest = line[:i], line[i+1:]
	} else {
		kw, rest = line, ""
	}
	rest = strings.TrimSpace(rest)
	ins := Instruction{Cmd: strings.ToLower(kw), Original: line, Line: lineNo}
	fail := func(format string, a ...any) (Instruction, error) {
		return Instruction{}, &Error{lineNo, strings.ToUpper(ins.Cmd) + ": " + fmt.Sprintf(format, a...)}
	}
	if !knownCmds[ins.Cmd] {
		return Instruction{}, &Error{lineNo, fmt.Sprintf("unknown instruction %q", kw)}
	}
	if flagCmds[ins.Cmd] {
		for strings.HasPrefix(rest, "--") {
			end := strings.IndexAny(rest, " \t")
			if end < 0 {
				end = len(rest)
			}
			f := rest[:end]
			if f == "--" {
				return fail("invalid flag %q", f)
			}
			ins.Flags = append(ins.Flags, f)
			rest = strings.TrimSpace(rest[end:])
		}
	}

	switch ins.Cmd {
	case "run", "cmd", "entrypoint":
		if args, ok := parseJSON(rest); ok {
			ins.Args, ins.JSON = args, true
		} else {
			ins.Args = []string{rest}
		}
		if ins.Cmd == "run" && rest == "" {
			return fail("requires a command")
		}
	case "shell":
		args, ok := parseJSON(rest)
		if !ok || len(args) == 0 {
			return fail(`requires the JSON form, e.g. SHELL ["/bin/sh", "-c"]`)
		}
		ins.Args, ins.JSON = args, true
	case "copy", "add":
		if args, ok := parseJSON(rest); ok {
			ins.Args, ins.JSON = args, true
		} else {
			ins.Args = splitWords(rest, esc)
		}
		if len(ins.Args) < 2 {
			return fail("requires at least one source and a destination")
		}
	case "volume":
		if args, ok := parseJSON(rest); ok {
			ins.Args, ins.JSON = args, true
		} else {
			ins.Args = splitWords(rest, esc)
		}
		if len(ins.Args) == 0 {
			return fail("requires at least one path")
		}
	case "env", "label":
		pairs, err := parseNameVal(rest, esc)
		if err != nil {
			return fail("%v", err)
		}
		ins.Args = pairs
	case "arg":
		words := splitWords(rest, esc)
		if len(words) == 0 {
			return fail("requires a name")
		}
		ins.Args = words
	case "from":
		ins.Args = splitWords(rest, esc)
		switch {
		case len(ins.Args) == 1:
		case len(ins.Args) == 3 && strings.EqualFold(ins.Args[1], "as"):
		default:
			return fail("expected FROM [--platform=...] <image> [AS <name>]")
		}
	case "expose":
		ins.Args = splitWords(rest, esc)
		if len(ins.Args) == 0 {
			return fail("requires at least one port")
		}
	case "healthcheck":
		typ, cmd, _ := strings.Cut(rest, " ")
		switch strings.ToUpper(typ) {
		case "NONE":
			if strings.TrimSpace(cmd) != "" || len(ins.Flags) > 0 {
				return fail("NONE takes no arguments")
			}
			ins.Args = []string{"NONE"}
		case "CMD":
			cmd = strings.TrimSpace(cmd)
			if args, ok := parseJSON(cmd); ok {
				ins.Args, ins.JSON = append([]string{"CMD"}, args...), true
			} else if cmd != "" {
				ins.Args = []string{"CMD", cmd}
			} else {
				return fail("CMD requires a command")
			}
		default:
			return fail("expected NONE or CMD")
		}
	default: // workdir, user, maintainer, stopsignal, onbuild
		if rest == "" {
			return fail("requires an argument")
		}
		ins.Args = []string{rest}
	}
	return ins, nil
}

// parseJSON parses s as a JSON array of strings.
func parseJSON(s string) ([]string, bool) {
	if !strings.HasPrefix(s, "[") {
		return nil, false
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, false
	}
	return out, true
}

// splitWords splits s on unquoted whitespace, keeping quotes and escapes
// in the words so that ProcessWord can interpret them later.
func splitWords(s string, esc rune) []string {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == esc && quote != '\'' && i+1 < len(rs):
			cur.WriteRune(c)
			cur.WriteRune(rs[i+1])
			i++
			inWord = true
		case quote != 0:
			cur.WriteRune(c)
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
			cur.WriteRune(c)
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// parseNameVal parses ENV/LABEL arguments into alternating key, raw value.
func parseNameVal(rest string, esc rune) ([]string, error) {
	words := splitWords(rest, esc)
	if len(words) == 0 {
		return nil, fmt.Errorf("requires at least one key=value pair")
	}
	if !strings.Contains(words[0], "=") {
		// Legacy form: "ENV key value with spaces".
		i := strings.IndexAny(rest, " \t")
		if i < 0 {
			return nil, fmt.Errorf("%q has no value (use key=value)", rest)
		}
		return []string{rest[:i], strings.TrimSpace(rest[i:])}, nil
	}
	out := make([]string, 0, 2*len(words))
	for _, w := range words {
		k, v, ok := strings.Cut(w, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%q is not a key=value pair", w)
		}
		out = append(out, k, v)
	}
	return out, nil
}
