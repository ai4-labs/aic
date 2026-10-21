package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `schemaVersion: 1
name: "demo"
version: "0.1.0"
toolchains: {}
targets:
  - darwin-arm64
  - linux-amd64
include:
  - "src/**"
entrypoint:
  cmd: [./src/start, --flag]
  env:
    GREETING: "hi"
`

func TestParseGood(t *testing.T) {
	p, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "demo" || len(p.Targets) != 2 || p.Entrypoint.WorkDir != DefaultWorkDir ||
		p.Entrypoint.Cmd[0] != "./src/start" || p.Entrypoint.Env["GREETING"] != "hi" {
		t.Fatalf("unexpected project: %+v", p)
	}
	if !p.Supports("darwin-arm64") || p.Supports("linux-arm64") {
		t.Fatal("Supports is wrong")
	}
}

func TestParseRejects(t *testing.T) {
	replace := func(old, new string) string { return strings.Replace(good, old, new, 1) }
	tests := map[string]struct{ in, want string }{
		"empty":          {"", "empty"},
		"two docs":       {good + "---\nname: x\n", "multiple YAML documents"},
		"anchor":         {replace(`name: "demo"`, `name: &n "demo"`), "anchors"},
		"alias":          {good + "extra: *n\n", "anchor"},
		"tag":            {replace(`version: "0.1.0"`, `version: !!str 0.1.0`), "explicit tags"},
		"duplicate key":  {good + "name: \"again\"\n", "duplicate key"},
		"unknown key":    {good + "config: {}\n", "not found"},
		"float version":  {replace(`version: "0.1.0"`, `version: 3.10`), "expected a string"},
		"bool name":      {replace(`name: "demo"`, `name: true`), "expected a string"},
		"schema":         {replace("schemaVersion: 1", "schemaVersion: 2"), "schemaVersion"},
		"bad platform":   {replace("- linux-amd64", "- windows-amd64"), "unsupported platform"},
		"abs include":    {replace(`"src/**"`, `"/etc/**"`), "relative"},
		"dotdot include": {replace(`"src/**"`, `"../**"`), "invalid segment"},
		"no cmd":         {replace("cmd: [./src/start, --flag]", "cmd: []"), "entrypoint.cmd"},
		"reserved env":   {replace("GREETING:", "PATH:"), "reserved"},
		"bad workdir":    {replace("  cmd:", "  workdir: ../x\n  cmd:"), "workdir"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tt.in))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		pat, name string
		want      bool
	}{
		{"src/**", "src/a.sh", true},
		{"src/**", "src/x/y/z.py", true},
		{"src/**", "srcx/a.sh", false},
		{"src/**", "other/a.sh", false},
		{"**/*.py", "a.py", true},
		{"**/*.py", "x/y/a.py", true},
		{"**/*.py", "x/y/a.sh", false},
		{"start.sh", "start.sh", true},
		{"start.sh", "x/start.sh", false},
		{"src/*.sh", "src/a.sh", true},
		{"src/*.sh", "src/x/a.sh", false},
	}
	for _, tt := range tests {
		if got := Match(tt.pat, tt.name); got != tt.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tt.pat, tt.name, got, tt.want)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "no aic.yaml") {
		t.Fatalf("missing file: err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p.Dir) {
		t.Fatalf("Dir = %q, want absolute", p.Dir)
	}
}
