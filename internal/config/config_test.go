package config

import "testing"

func TestDenyAll(t *testing.T) {
	p := DenyAll()
	if len(p.Network.Egress) != 0 || len(p.Filesystem.Writable) != 0 {
		t.Fatalf("DenyAll grants something: %+v", p)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParse(t *testing.T) {
	p, err := Parse([]byte(`{"network":{"egress":["api.openai.com:443"]},"maxProcs":8}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.MaxProcs != 8 || p.Network.Egress[0] != "api.openai.com:443" {
		t.Fatalf("unexpected config: %+v", p)
	}
	for _, bad := range []string{
		`{"netwrok":{"egress":["x:1"]}}`,               // typo'd key
		`{"network":{"egress":["*.example.com:443"]}}`, // invalid entry
		`{"maxProcs":1} {"maxProcs":2}`,                // trailing data
		`not json`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%s) succeeded, want error", bad)
		}
	}
}

func TestValidate(t *testing.T) {
	good := Config{
		Network:    Network{Egress: []string{"api.openai.com:443", "10.0.0.1:8080", "[::1]:9000"}},
		Filesystem: Filesystem{Writable: []string{"$WORKSPACE/out", "$SCRATCH"}},
		MaxMemory:  16 << 30,
		MaxProcs:   64,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate(good) = %v", err)
	}

	bad := []Config{
		{Network: Network{Egress: []string{"api.openai.com"}}},
		{Network: Network{Egress: []string{"*.openai.com:443"}}},
		{Network: Network{Egress: []string{"host:0"}}},
		{Network: Network{Egress: []string{"host:99999"}}},
		{Filesystem: Filesystem{Writable: []string{"/etc"}}},
		{Filesystem: Filesystem{Writable: []string{"$WORKSPACE/../.."}}},
		{MaxMemory: -1},
		{MaxProcs: -1},
	}
	for i, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("bad[%d]: Validate = nil, want error", i)
		}
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1024", 1024, true},
		{"16GiB", 16 << 30, true},
		{"512 MiB", 512 << 20, true},
		{"2GB", 2e9, true},
		{"10B", 10, true},
		{"", 0, false},
		{"-1GiB", 0, false},
		{"lots", 0, false},
		{"9999999999TiB", 0, false},
	}
	for _, tt := range tests {
		got, err := ParseSize(tt.in)
		if tt.ok != (err == nil) || got != tt.want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d, ok=%v", tt.in, got, err, tt.want, tt.ok)
		}
	}
}
