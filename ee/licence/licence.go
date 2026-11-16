// Package licence is Canyon Enterprise's licence file (contracts/schema/licence.schema.json; contracts/routes.md,
// Licence): a payload and Canyon's Ed25519 signature over its exact bytes, both in base64, verified offline.
package licence

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Grace is how long after its expiry a licence still unlocks its features.
const Grace = 30 * 24 * time.Hour

// Layout is how the payload writes an instant: RFC 3339 at whole seconds UTC, the contract's ts.
const Layout = "2006-01-02T15:04:05Z"

// The problems of a licence that is set but unusable (the schema's state.problem).
const (
	Unreadable = "unreadable" // not a licence file, or a payload that is not the contract's
	Unsigned   = "unsigned"   // no key verifies it
)

// Payload is what Canyon signs.
type Payload struct {
	Customer        string
	Issued, Expires time.Time
	Seats           int
	Features        []string
}

// file is the licence file: exactly these two fields.
type file struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// wire is the payload as its bytes spell it.
type wire struct {
	Customer string   `json:"customer"`
	Issued   string   `json:"issued"`
	Expires  string   `json:"expires"`
	Seats    int      `json:"seats"`
	Features []string `json:"features"`
}

var featureName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// Sign is the licence file for p, signed with key: the payload's JSON bytes and the signature over them.
func Sign(key ed25519.PrivateKey, p Payload) ([]byte, error) {
	raw, err := json.Marshal(wire{p.Customer, p.Issued.UTC().Format(Layout), p.Expires.UTC().Format(Layout), p.Seats, p.Features})
	if err != nil {
		return nil, err
	}
	if _, err := parse(raw); err != nil {
		return nil, err
	}
	return json.Marshal(file{base64.StdEncoding.EncodeToString(raw), base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))})
}

// Verify reads text as a licence file signed by one of keys. A text that is not a licence file, or whose signed
// payload is not the contract's, is Unreadable; one no key verifies is Unsigned. The signature is checked before
// the payload is parsed, so nothing unsigned is read further than its base64.
func Verify(text []byte, keys []ed25519.PublicKey) (Payload, string) {
	var f file
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.DisallowUnknownFields()
	if dec.Decode(&f) != nil || dec.Decode(&struct{}{}) != io.EOF { //nolint:errorlint // sentinel from the decoder itself
		return Payload{}, Unreadable
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(f.Payload)
	if err != nil || len(raw) == 0 || len(raw) > 4096 {
		return Payload{}, Unreadable
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(f.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Payload{}, Unreadable
	}
	if !slices.ContainsFunc(keys, func(k ed25519.PublicKey) bool { return len(k) == ed25519.PublicKeySize && ed25519.Verify(k, raw, sig) }) {
		return Payload{}, Unsigned
	}
	p, err := parse(raw)
	if err != nil {
		return Payload{}, Unreadable
	}
	return p, ""
}

// parse reads a payload's bytes as the contract's payload, or refuses them.
func parse(raw []byte) (Payload, error) {
	var w wire
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return Payload{}, err
	}
	if dec.Decode(&struct{}{}) != io.EOF { //nolint:errorlint // sentinel from the decoder itself
		return Payload{}, errors.New("trailing data after the payload")
	}
	issued, err := time.Parse(Layout, w.Issued)
	if err != nil {
		return Payload{}, err
	}
	expires, err := time.Parse(Layout, w.Expires)
	if err != nil {
		return Payload{}, err
	}
	switch {
	case !plain(w.Customer, 128):
		return Payload{}, errors.New("customer is 1 to 128 characters of plain text")
	case !expires.After(issued):
		return Payload{}, errors.New("expires is after issued")
	case w.Seats < 1 || w.Seats > 1_000_000:
		return Payload{}, errors.New("seats is 1 to 1,000,000")
	case len(w.Features) == 0 || len(w.Features) > 32:
		return Payload{}, errors.New("features names 1 to 32 features")
	}
	for i, f := range w.Features {
		if !featureName.MatchString(f) || slices.Contains(w.Features[:i], f) {
			return Payload{}, errors.New("a feature is a lower-case name, each once")
		}
	}
	return Payload{w.Customer, issued, expires, w.Seats, w.Features}, nil
}

// plain is 1 to max characters with nothing that hides or reorders text, and not only space.
func plain(s string, max int) bool {
	if n := utf8.RuneCountInString(s); n == 0 || n > max || strings.TrimSpace(s) == "" {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return r == 0x7F || unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) })
}

// State is the licence's state at now: valid before it expires, grace for Grace after, lapsed from then on.
func (p Payload) State(now time.Time) string {
	switch {
	case now.Before(p.Expires):
		return "valid"
	case now.Before(p.Expires.Add(Grace)):
		return "grace"
	}
	return "lapsed"
}
