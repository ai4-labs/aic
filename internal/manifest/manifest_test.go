package manifest

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rakyll/ai-container/internal/digest"
)

func d(s string) digest.Digest { return digest.FromBytes([]byte(s)) }

func validManifest() *Manifest {
	return &Manifest{
		SchemaVersion: SchemaVersion,
		MediaType:     MediaType,
		Name:          "summarizer",
		Version:       "0.1.0",
		Lock:          d("lock"),
		Components: []Component{
			{Kind: KindCode, Name: "app", Tree: d("code"), Mount: "app", Size: 10},
			{Kind: KindData, Name: "fixtures", Tree: d("data"), Mount: "data/fixtures", Size: 100},
		},
		Targets: map[Platform]Target{
			"darwin-arm64": {Components: []Component{
				{Kind: KindRuntime, Name: "node", Tree: d("node-darwin"), Mount: "runtime/node",
					Path: []string{"bin"}},
				{Kind: KindPackages, Name: "npm", Tree: d("npm-darwin"), Mount: "runtime/node_modules",
					Env: map[string]string{"NODE_PATH": "${AIC_MOUNT}"}},
			}},
			"linux-amd64": {Components: []Component{
				{Kind: KindRuntime, Name: "server", Tree: d("server-linux"), Mount: "runtime/server",
					Path: []string{"bin"}},
				{Kind: KindPackages, Name: "libs", Tree: d("libs-linux"), Mount: "runtime/libs",
					Env: map[string]string{"LD_LIBRARY_PATH": "${AIC_MOUNT}/lib"}},
			}},
		},
		Entrypoint: Entrypoint{Cmd: []string{"node", "app/index.js"}, Env: map[string]string{"LOG_LEVEL": "info"}},
	}
}

func TestValidateOK(t *testing.T) {
	if err := validManifest().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{"schema", func(m *Manifest) { m.SchemaVersion = 2 }, "schemaVersion"},
		{"name", func(m *Manifest) { m.Name = "Bad Name" }, "invalid name"},
		{"no lock", func(m *Manifest) { m.Lock = digest.Digest{} }, "lock digest"},
		{"no cmd", func(m *Manifest) { m.Entrypoint.Cmd = nil }, "entrypoint.cmd"},
		{"no targets", func(m *Manifest) { m.Targets = nil }, "at least one target"},
		{"bad platform", func(m *Manifest) { m.Targets["windows-amd64"] = Target{} }, "invalid target platform"},
		{"runtime shared", func(m *Manifest) {
			m.Components = append(m.Components, Component{Kind: KindRuntime, Name: "py", Tree: d("x"), Mount: "rt"})
		}, "not allowed here"},
		{"code in target", func(m *Manifest) {
			t := m.Targets["linux-amd64"]
			t.Components = append(t.Components, Component{Kind: KindCode, Name: "c", Tree: d("x"), Mount: "c"})
			m.Targets["linux-amd64"] = t
		}, "not allowed here"},
		{"unknown kind", func(m *Manifest) { m.Components[1].Kind = "weights" }, "unknown component kind"},
		{"reserved scratch mount", func(m *Manifest) { m.Components[0].Mount = "scratch" }, "reserved"},
		{"reserved in target", func(m *Manifest) {
			m.Targets["darwin-arm64"].Components[0].Mount = "workspace/py"
		}, "reserved"},
		{"escape mount", func(m *Manifest) { m.Components[0].Mount = "../etc" }, "invalid mount"},
		{"abs mount", func(m *Manifest) { m.Components[0].Mount = "/etc" }, "invalid mount"},
		{"dup mount", func(m *Manifest) { m.Components[1].Mount = "app" }, "already used"},
		{"target mount clash", func(m *Manifest) {
			m.Targets["darwin-arm64"].Components[0].Mount = "data/fixtures"
		}, "already used"},
		{"dup component", func(m *Manifest) { m.Components[1].Kind, m.Components[1].Name = KindCode, "app" }, "duplicate component"},
		{"path escape", func(m *Manifest) { m.Targets["darwin-arm64"].Components[0].Path = []string{"../../bin"} }, "invalid path entry"},
		{"path absolute", func(m *Manifest) { m.Targets["darwin-arm64"].Components[0].Path = []string{"/usr/bin"} }, "invalid path entry"},
		{"env PATH reserved", func(m *Manifest) { m.Components[0].Env = map[string]string{"PATH": "/x"} }, "reserved"},
		{"env AIC_ reserved", func(m *Manifest) { m.Components[0].Env = map[string]string{"AIC_ROOT": "/x"} }, "reserved"},
		{"env bad name", func(m *Manifest) { m.Components[0].Env = map[string]string{"1BAD": "x"} }, "invalid env name"},
		{"entrypoint env reserved", func(m *Manifest) { m.Entrypoint.Env = map[string]string{"HOME": "/"} }, "reserved"},
		{"env conflict shared vs target", func(m *Manifest) {
			m.Components[0].Env = map[string]string{"NODE_PATH": "x"}
		}, "already set by"},
		{"env conflict within target", func(m *Manifest) {
			m.Targets["linux-amd64"].Components[0].Env = map[string]string{"LD_LIBRARY_PATH": "x"}
		}, "already set by"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := validManifest()
			tt.mutate(m)
			err := m.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestParseRoundTrip(t *testing.T) {
	b, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "summarizer" || len(m.Targets) != 2 {
		t.Fatalf("unexpected parse result: %+v", m)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	b, _ := json.Marshal(validManifest())
	for _, field := range []string{`"surprise":1`, `"config":"sha256:00"`} {
		in := append([]byte("{"+field+","), b[1:]...)
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse with %s: expected unknown-field error", field)
		}
	}
}

func TestParseRejectsUnknownTargetFields(t *testing.T) {
	b, _ := json.Marshal(validManifest())
	for _, field := range []string{`"requires":["x>=1"]`, `"surprise":true`} {
		in := bytes.Replace(b, []byte(`"linux-amd64":{`), []byte(`"linux-amd64":{`+field+`,`), 1)
		if bytes.Equal(in, b) {
			t.Fatal("fixture did not contain linux-amd64 target")
		}
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse with target field %s: expected unknown-field error", field)
		}
	}
}

func TestHostPlatformValid(t *testing.T) {
	// CI runs on darwin/linux amd64/arm64 only.
	if p := HostPlatform(); !p.Valid() {
		t.Skipf("host platform %q is not a supported target", p)
	}
}
