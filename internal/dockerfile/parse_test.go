package dockerfile

import (
	"reflect"
	"strings"
	"testing"
)

func parse(t *testing.T, src string) *Dockerfile {
	t.Helper()
	df, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return df
}

func TestParseBasics(t *testing.T) {
	df := parse(t, `# syntax=docker/dockerfile:1
# a comment
ARG BASE=alpine
FROM --platform=linux/amd64 ${BASE} AS build
RUN echo one \
    two \
# comment inside continuation

    three
RUN ["echo", "exec form"]
COPY --chown=1:2 a b /dst/
ENV A=1 B="two words" C=x\ y
ENV LEGACY some value
LABEL "com.example"="v"
CMD echo hi
HEALTHCHECK --interval=5s CMD ["true"]
SHELL ["/bin/bash", "-c"]
`)
	got := df.Instructions
	if len(got) != 11 {
		t.Fatalf("got %d instructions, want 11: %+v", len(got), got)
	}
	check := func(i int, cmd string, args []string, json bool) {
		t.Helper()
		if got[i].Cmd != cmd || !reflect.DeepEqual(got[i].Args, args) || got[i].JSON != json {
			t.Errorf("instruction %d = %s %q json=%v, want %s %q json=%v", i, got[i].Cmd, got[i].Args, got[i].JSON, cmd, args, json)
		}
	}
	check(0, "arg", []string{"BASE=alpine"}, false)
	check(1, "from", []string{"${BASE}", "AS", "build"}, false)
	if v, ok := got[1].Flag("platform"); !ok || v != "linux/amd64" {
		t.Errorf("platform flag = %q, %v", v, ok)
	}
	check(2, "run", []string{"echo one     two     three"}, false)
	if got[2].Line != 5 {
		t.Errorf("RUN line = %d, want 5", got[2].Line)
	}
	check(3, "run", []string{"echo", "exec form"}, true)
	check(4, "copy", []string{"a", "b", "/dst/"}, false)
	check(5, "env", []string{"A", "1", "B", `"two words"`, "C", `x\ y`}, false)
	check(6, "env", []string{"LEGACY", "some value"}, false)
	check(7, "label", []string{`"com.example"`, `"v"`}, false)
	check(8, "cmd", []string{"echo hi"}, false)
	check(9, "healthcheck", []string{"CMD", "true"}, true)
	check(10, "shell", []string{"/bin/bash", "-c"}, true)
}

func TestParseEscapeDirective(t *testing.T) {
	df := parse(t, "# escape=`\nFROM scratch\nRUN dir `\n  c:\\\n")
	if df.Escape != '`' {
		t.Fatalf("escape = %q", df.Escape)
	}
	if got := df.Instructions[1].Args[0]; got != `dir   c:\` {
		t.Errorf("RUN = %q", got)
	}
}

func TestParseHeredocs(t *testing.T) {
	df := parse(t, `FROM alpine
RUN <<EOF
echo a
echo b
EOF
RUN cat <<-"END" > /f
	keep $X
	END
COPY <<A <<B /dir/
alpha
A
beta
B
CMD ["x"]
`)
	ins := df.Instructions
	if len(ins) != 5 {
		t.Fatalf("got %d instructions: %+v", len(ins), ins)
	}
	if got := ins[1].Args[0]; got != "echo a\necho b\n" {
		t.Errorf("RUN heredoc script = %q", got)
	}
	if got, want := ins[2].Args[0], "cat <<-\"END\" > /f\n\tkeep $X\n\tEND"; got != want {
		t.Errorf("RUN with heredoc = %q, want %q", got, want)
	}
	if h := ins[2].Heredocs[0]; h.Content != "keep $X\n" || h.Expand {
		t.Errorf("heredoc = %+v", h)
	}
	if hs := ins[3].Heredocs; len(hs) != 2 || hs[0].Content != "alpha\n" || hs[1].Content != "beta\n" || !hs[0].Expand {
		t.Errorf("COPY heredocs = %+v", hs)
	}
	if ins[4].Cmd != "cmd" {
		t.Errorf("last = %+v", ins[4])
	}

	// Shifts like "$((1<<n))", "<<" in quotes and here-strings are not
	// heredocs, and quoted COPY paths with spaces stay intact.
	df2 := parse(t, `FROM alpine
RUN echo $((1<<n)) && python3 -c 'print(1<<shift)' && echo "<<NOT_A_HEREDOC" && cat <<<str
COPY "my file.txt" a\ b.txt "/dst dir/"
`)
	if len(df2.Instructions) != 3 {
		t.Fatalf("got %d instructions: %+v", len(df2.Instructions), df2.Instructions)
	}
	if len(df2.Instructions[1].Heredocs) != 0 {
		t.Errorf("unexpected heredocs: %+v", df2.Instructions[1].Heredocs)
	}
	if want := []string{`"my file.txt"`, `a\ b.txt`, `"/dst dir/"`}; !reflect.DeepEqual(df2.Instructions[2].Args, want) {
		t.Errorf("COPY args = %q, want %q", df2.Instructions[2].Args, want)
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{
		"FROM a b",
		"BOGUS x",
		"RUN",
		"COPY onlyone",
		"SHELL /bin/sh -c",
		"ENV novalue",
		"FROM a\nRUN <<EOF\nnever ends",
		"# escape=x\nFROM a",
	} {
		if _, err := Parse(strings.NewReader(src)); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", src)
		}
	}
}

func TestProcessWord(t *testing.T) {
	env := map[string]string{"A": "aa", "EMPTY": "", "P": "/x"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	for _, tc := range []struct{ in, want string }{
		{"$A", "aa"},
		{"${A}b", "aab"},
		{"$A$A", "aaaa"},
		{"${UNSET:-def}", "def"},
		{"${EMPTY:-def}", "def"},
		{"${EMPTY-def}", ""},
		{"${A:+alt}", "alt"},
		{"${UNSET:+alt}", ""},
		{"${UNSET:-${A}x}", "aax"},
		{`'$A'`, "$A"},
		{`"$A b"`, "aa b"},
		{`\$A`, "$A"},
		{`a\ b`, "a b"},
		{`"q\"x"`, `q"x`},
		{"$", "$"},
		{"$1x", "$1x"},
		{"cost: 5$", "cost: 5$"},
		{"${P}/bin", "/x/bin"},
	} {
		got, err := ProcessWord(tc.in, lookup, '\\')
		if err != nil || got != tc.want {
			t.Errorf("ProcessWord(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := ProcessWord("${UNSET:?must be set}", lookup, '\\'); err == nil || !strings.Contains(err.Error(), "must be set") {
		t.Errorf("want error for :?, got %v", err)
	}
	for _, bad := range []string{"${A", "'open", `"open`, "${}", "${A/x}"} {
		if _, err := ProcessWord(bad, lookup, '\\'); err == nil {
			t.Errorf("ProcessWord(%q) succeeded, want error", bad)
		}
	}
	if got, _ := ExpandVars(`"$A" \x`, lookup, '\\'); got != `"aa" \x` {
		t.Errorf("ExpandVars = %q", got)
	}
}
