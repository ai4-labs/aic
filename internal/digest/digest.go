// Package digest implements content digests used to address every object
// (blob, chunk, tree, manifest) in an aic bundle.
//
// The canonical string form is "<algorithm>:<lowercase hex>", e.g.
// "sha256:e3b0c442...". Only sha256 is supported in format version 1.
package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// Algorithm identifies a hash function.
type Algorithm string

// SHA256 is the only algorithm defined by format version 1.
const SHA256 Algorithm = "sha256"

// Digest is a validated content digest. The zero value is invalid.
type Digest struct {
	alg Algorithm
	hex string
}

// ErrInvalid is returned when a digest string cannot be parsed.
var ErrInvalid = errors.New("digest: invalid")

// FromBytes returns the sha256 digest of b.
func FromBytes(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest{alg: SHA256, hex: hex.EncodeToString(sum[:])}
}

// FromReader returns the sha256 digest of everything read from r and the
// number of bytes read.
func FromReader(r io.Reader) (Digest, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return Digest{}, n, err
	}
	return Digest{alg: SHA256, hex: hex.EncodeToString(h.Sum(nil))}, n, nil
}

// Parse parses and validates the canonical string form of a digest.
func Parse(s string) (Digest, error) {
	alg, h, ok := strings.Cut(s, ":")
	if !ok {
		return Digest{}, fmt.Errorf("%w: %q: missing algorithm", ErrInvalid, s)
	}
	if Algorithm(alg) != SHA256 {
		return Digest{}, fmt.Errorf("%w: %q: unsupported algorithm %q", ErrInvalid, s, alg)
	}
	if len(h) != sha256.Size*2 {
		return Digest{}, fmt.Errorf("%w: %q: want %d hex chars, got %d", ErrInvalid, s, sha256.Size*2, len(h))
	}
	for _, c := range h {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return Digest{}, fmt.Errorf("%w: %q: hex must be lowercase [0-9a-f]", ErrInvalid, s)
		}
	}
	return Digest{alg: SHA256, hex: h}, nil
}

// Algorithm returns the digest algorithm.
func (d Digest) Algorithm() Algorithm { return d.alg }

// Hex returns the lowercase hex-encoded hash.
func (d Digest) Hex() string { return d.hex }

// IsZero reports whether d is the zero (invalid) value.
func (d Digest) IsZero() bool { return d.alg == "" }

// String returns the canonical "<alg>:<hex>" form.
func (d Digest) String() string {
	if d.IsZero() {
		return ""
	}
	return string(d.alg) + ":" + d.hex
}

// Path returns the relative object path for d under a store or bundle
// directory, e.g. "blobs/sha256/e3b0...". kind is "blobs" or "trees".
func (d Digest) Path(kind string) string {
	return path.Join(kind, string(d.alg), d.hex)
}

// MarshalText implements encoding.TextMarshaler.
func (d Digest) MarshalText() ([]byte, error) {
	if d.IsZero() {
		return nil, fmt.Errorf("%w: zero digest", ErrInvalid)
	}
	return []byte(d.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Digest) UnmarshalText(b []byte) error {
	p, err := Parse(string(b))
	if err != nil {
		return err
	}
	*d = p
	return nil
}
