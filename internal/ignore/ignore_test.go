package ignore

import (
	"strings"
	"testing"
)

func TestExcluded(t *testing.T) {
	m, err := Parse(strings.NewReader(`
# comment
node_modules
*.log
!keep.log
**/tmp
/build/
docs/**/*.md
!docs/README.md
file[0-9].txt
`))
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]bool{
		"node_modules":           true,
		"node_modules/x/y.js":    true,
		"src/node_modules":       false,
		"a.log":                  true,
		"keep.log":               false,
		"sub/a.log":              false, // *.log is anchored at the root
		"tmp":                    true,
		"a/b/tmp/c":              true,
		"build":                  true,
		"build/out":              true,
		"docs/a.md":              true,
		"docs/x/y/a.md":          true,
		"docs/README.md":         false,
		"docs/a.txt":             false,
		"file1.txt":              true,
		"filex.txt":              false,
		"main.go":                false,
		"":                       false,
		"node_modulesX/file.txt": false,
	} {
		if got := m.Excluded(rel); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", rel, got, want)
		}
	}
	if !m.HasExceptions() {
		t.Error("HasExceptions = false")
	}
	var nilM *Matcher
	if nilM.Excluded("x") {
		t.Error("nil matcher excluded a path")
	}
}
