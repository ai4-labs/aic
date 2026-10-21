package digest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const emptySHA256 = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestFromBytesAndReader(t *testing.T) {
	if got := FromBytes(nil).String(); got != emptySHA256 {
		t.Fatalf("FromBytes(nil) = %s, want %s", got, emptySHA256)
	}
	d, n, err := FromReader(strings.NewReader(""))
	if err != nil || n != 0 || d.String() != emptySHA256 {
		t.Fatalf("FromReader(\"\") = %s, %d, %v", d, n, err)
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{emptySHA256, true},
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", false},
		{"md5:d41d8cd98f00b204e9800998ecf8427e", false},
		{"sha256:abc", false},
		{strings.ToUpper(emptySHA256[:7]) + emptySHA256[7:], false},
		{"sha256:E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855", false},
		{"", false},
	}
	for _, tt := range tests {
		d, err := Parse(tt.in)
		if tt.ok {
			if err != nil || d.String() != tt.in {
				t.Errorf("Parse(%q) = %v, %v; want ok", tt.in, d, err)
			}
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) err = %v; want ErrInvalid", tt.in, err)
		}
	}
}

func TestPath(t *testing.T) {
	d := FromBytes(nil)
	want := "blobs/sha256/" + d.Hex()
	if got := d.Path("blobs"); got != want {
		t.Fatalf("Path = %s, want %s", got, want)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	type wrap struct {
		D Digest `json:"d"`
	}
	b, err := json.Marshal(wrap{FromBytes(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"d":"`+emptySHA256+`"}` {
		t.Fatalf("marshal = %s", b)
	}
	var w wrap
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	if w.D.String() != emptySHA256 {
		t.Fatalf("round trip = %s", w.D)
	}
	if err := json.Unmarshal([]byte(`{"d":"sha256:nope"}`), &w); err == nil {
		t.Fatal("expected error for invalid digest")
	}
	if _, err := json.Marshal(wrap{}); err == nil {
		t.Fatal("expected error marshaling zero digest")
	}
}
