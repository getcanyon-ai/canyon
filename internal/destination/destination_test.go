package destination

import (
	"errors"
	"strings"
	"testing"
)

var (
	label63 = strings.Repeat("a", 63)
	name253 = strings.Repeat(label63+".", 3) + strings.Repeat("b", 61) // 4·63 + 61 = 253
)

// The table of design/EGRESS.md §3.3: every rule a row that keeps a name and a row that refuses one,
// every IP-literal encoding a client reads as an address among the refused, each for its own reason.
func TestCanonical(t *testing.T) {
	for _, c := range []struct {
		authority string
		port      int
		name      string
		want      int
		err       error
	}{
		{"api.stripe.com:443", 443, "api.stripe.com", 443, nil},
		{"api.stripe.com:80", 443, "api.stripe.com", 80, nil},
		{"api.stripe.com", 80, "api.stripe.com", 80, nil},
		{"api.stripe.com", 443, "api.stripe.com", 443, nil},
		// lower-case; one trailing dot dropped; IDNA to A-labels
		{"API.Stripe.COM:443", 443, "api.stripe.com", 443, nil},
		{"api.stripe.com.:443", 443, "api.stripe.com", 443, nil},
		{"api.stripe.com..:443", 443, "", 0, ErrName},
		{"xn--bcher-kva.de:443", 443, "xn--bcher-kva.de", 443, nil},
		{"XN--BCHER-KVA.DE.:443", 443, "xn--bcher-kva.de", 443, nil},
		{"api.stripe.com。:443", 443, "", 0, ErrName}, // the dot dropped is one ASCII dot, before IDNA maps this one
		{"a_b.example.com:443", 443, "", 0, ErrName},
		{".stripe.com:443", 443, "", 0, ErrName},
		{"-api.stripe.com:443", 443, "", 0, ErrName},
		{"api-.stripe.com:443", 443, "", 0, ErrName},
		{"xn--a.example.com:443", 443, "", 0, ErrName}, // punycode that decodes to no label
		// a host UTS 46 maps to another name is refused: Envoy would dial the one it was given
		{"bücher.de:443", 443, "", 0, ErrMapped},
		{"faß.de:443", 443, "", 0, ErrMapped},
		{"İ.example.com:443", 443, "", 0, ErrMapped},
		{"ＡＰＩ.example.com:443", 443, "", 0, ErrMapped},
		{"api.stripe.com\u200b:443", 443, "", 0, ErrMapped},
		{"①.example.com:443", 443, "", 0, ErrMapped},
		{"\u212aa.example.com:443", 443, "", 0, ErrMapped}, // the Kelvin sign, which Unicode lower-cases to k
		{"*.stripe.com:443", 443, "", 0, ErrName},
		{"a..example.com:443", 443, "", 0, ErrName},
		{"", 443, "", 0, ErrName},
		{":443", 443, "", 0, ErrName},
		// userinfo refused
		{"user@api.stripe.com:443", 443, "", 0, ErrUserinfo},
		{"user:x@api.stripe.com:443", 443, "", 0, ErrUserinfo},
		{"user@[::1]:443", 443, "", 0, ErrUserinfo},
		{"@api.stripe.com:443", 443, "", 0, ErrUserinfo},
		{"user@bücher.de:443", 443, "", 0, ErrUserinfo},
		// every IP-literal encoding refused: dotted, integer, hex, octal, bracketed, v4-mapped
		{"10.0.0.1:443", 443, "", 0, ErrIPLiteral},
		{"10.0.0.1", 80, "", 0, ErrIPLiteral},
		{"10.0.0.1.:443", 443, "", 0, ErrIPLiteral},
		{"169.254.169.254:80", 443, "", 0, ErrIPLiteral},
		{"10.1:443", 443, "", 0, ErrIPLiteral},
		{"167772161:443", 443, "", 0, ErrIPLiteral},
		{"0xa000001:443", 443, "", 0, ErrIPLiteral},
		{"0XA000001:443", 443, "", 0, ErrIPLiteral},
		{"0xa.0x0.0x0.0x1:443", 443, "", 0, ErrIPLiteral},
		{"api.0x:443", 443, "", 0, ErrIPLiteral},
		{"0300.0250.01.01:443", 443, "", 0, ErrIPLiteral},
		{"012.0.0.1:443", 443, "", 0, ErrIPLiteral},
		{"01200000001:443", 443, "", 0, ErrIPLiteral},
		{"１０.０.０.１:443", 443, "", 0, ErrIPLiteral},
		{"10。0。0。1:443", 443, "", 0, ErrIPLiteral},
		{"[::1]:443", 443, "", 0, ErrIPLiteral},
		{"[::1]", 443, "", 0, ErrIPLiteral},
		{"[::ffff:10.0.0.1]:443", 443, "", 0, ErrIPLiteral},
		{"::ffff:10.0.0.1", 443, "", 0, ErrIPLiteral},
		{"::ffff:10.0.0.1:443", 443, "", 0, ErrIPLiteral},
		{"::1", 443, "", 0, ErrIPLiteral},
		{"::ffff:10.0.0.1:80", 443, "", 0, ErrIPLiteral},
		{"fe80::1%eth0", 443, "", 0, ErrIPLiteral},
		{"fe80::1%eth0:443", 443, "", 0, ErrIPLiteral},
		// a number that is not the last label is a name like any other
		{"1.2.3.4.example.com:443", 443, "1.2.3.4.example.com", 443, nil},
		{"1password.com:443", 443, "1password.com", 443, nil},
		{"0x1f.example.com:443", 443, "0x1f.example.com", 443, nil},
		// single-label names refused
		{"localhost:443", 443, "", 0, ErrSingleLabel},
		{"metadata:80", 443, "", 0, ErrSingleLabel},
		{"canyon-server.:80", 443, "", 0, ErrSingleLabel},
		// labels ≤63, names ≤253
		{label63 + ".example.com:443", 443, label63 + ".example.com", 443, nil},
		{label63 + "a.example.com:443", 443, "", 0, ErrLength},
		{name253 + ":443", 443, name253, 443, nil},
		{name253 + "b:443", 443, "", 0, ErrLength},
		// ports 80 and 443 only
		{"api.stripe.com:8080", 443, "", 0, ErrPort},
		{"api.stripe.com:22", 443, "", 0, ErrPort},
		{"api.stripe.com:0443", 443, "", 0, ErrPort},
		{"api.stripe.com:+443", 443, "", 0, ErrPort},
		{"api.stripe.com:", 443, "", 0, ErrPort},
		{"api.stripe.com", 8080, "", 0, ErrPort},
	} {
		name, port, err := Canonical(c.authority, c.port)
		if name != c.name || port != c.want || !errors.Is(err, c.err) || (err == nil) != (c.err == nil) {
			t.Errorf("Canonical(%q, %d) = %q, %d, %v; want %q, %d, %v", c.authority, c.port, name, port, err, c.name, c.want, c.err)
		}
	}
}

// An allowlist entry is a name or *. and a name, canonicalised as a destination; a wildcard anywhere else,
// over a single label or over an IP literal is no entry.
func TestEntry(t *testing.T) {
	for _, c := range []struct {
		entry, want string
		err         error
	}{
		{"api.stripe.com", "api.stripe.com", nil},
		{"*.stripe.com", "*.stripe.com", nil},
		{"*.Bücher.DE.", "*.xn--bcher-kva.de", nil},
		{"Bücher.DE.", "xn--bcher-kva.de", nil},
		{"faß.de", "xn--fa-hia.de", nil}, // UTS 46 non-transitional: ß stays ß
		{"İ.example.com", "xn--i-9bb.example.com", nil},
		{"ＡＰＩ.example.com", "api.example.com", nil},
		{"*.0XA", "", ErrIPLiteral},
		{"xn--a.example.com", "", ErrName},
		{".stripe.com", "", ErrName},
		{"-api.stripe.com", "", ErrName},
		{"*.com", "", ErrSingleLabel},
		{"*", "", ErrName},
		{"*.", "", ErrName},
		{"*.*.stripe.com", "", ErrName},
		{"api.*.stripe.com", "", ErrName},
		{"**.stripe.com", "", ErrName},
		{"*.10.0.0.1", "", ErrIPLiteral},
		{"10.0.0.1", "", ErrIPLiteral},
		{"0300.0250.01.01", "", ErrIPLiteral},
		{"::1", "", ErrIPLiteral},
		{"[::1]", "", ErrName},
		{"user@api.stripe.com", "", ErrName},
		{"api.stripe.com:443", "", ErrName},
		{"*." + name253, "*." + name253, nil},
		{"*." + name253 + "b", "", ErrLength},
	} {
		got, err := Entry(c.entry)
		if got != c.want || !errors.Is(err, c.err) || (err == nil) != (c.err == nil) {
			t.Errorf("Entry(%q) = %q, %v; want %q, %v", c.entry, got, err, c.want, c.err)
		}
	}
}

// *.stripe.com allows any depth below stripe.com and never stripe.com; a name allows itself alone.
func TestAllows(t *testing.T) {
	for _, c := range []struct {
		entry, name string
		want        bool
	}{
		{"*.stripe.com", "api.stripe.com", true},
		{"*.stripe.com", "a.b.stripe.com", true},
		{"*.stripe.com", "stripe.com", false},
		{"*.stripe.com", "evilstripe.com", false},
		{"*.stripe.com", "api.stripe.com.evil.example", false},
		{"api.stripe.com", "api.stripe.com", true},
		{"api.stripe.com", "a.api.stripe.com", false},
		{"stripe.com", "api.stripe.com", false},
		{"*.", "api.stripe.com", false},
		{"*.xn--bcher-kva.de", "shop.xn--bcher-kva.de", true},
		{"*.xn--bcher-kva.de", "xn--bcher-kva.de", false},
	} {
		if got := Allows(c.entry, c.name); got != c.want {
			t.Errorf("Allows(%q, %q) = %v, want %v", c.entry, c.name, got, c.want)
		}
	}
}

// A record keeps the canonical name and whatever port was called, a dotted IPv4 address as it is (layer 1's
// records), and invalid on 443 for anything else the check refuses as no destination.
func TestRecorded(t *testing.T) {
	for _, c := range []struct {
		authority string
		port      int
		name      string
		want      int
	}{
		{"api.stripe.com:443", 443, "api.stripe.com", 443},
		{"API.Stripe.COM.", 80, "api.stripe.com", 80},
		{"api.stripe.com:8443", 443, "api.stripe.com", 8443},
		{"10.0.0.1:443", 443, "10.0.0.1", 443},
		{"10.0.0.1", 80, "10.0.0.1", 80},
		{"10.0.0.1:8080", 443, "10.0.0.1", 8080},
		{"0300.0250.1.1:443", 443, "invalid", 443},
		{"167772161:443", 443, "invalid", 443},
		{"[::1]:443", 443, "invalid", 443},
		{"[api.stripe.com]:443", 443, "invalid", 443},
		{"::ffff:10.0.0.1", 443, "invalid", 443},
		{"bücher.de:443", 443, "invalid", 443},
		{"user@api.stripe.com:443", 443, "invalid", 443},
		{"localhost:80", 80, "invalid", 443},
		{"api.stripe.com:0", 443, "invalid", 443},
		{"api.stripe.com:65536", 443, "invalid", 443},
		{"api.stripe.com:x", 443, "invalid", 443},
		{"api.stripe.com:0443", 443, "invalid", 443},
		{"api.stripe.com:+443", 443, "invalid", 443},
		{label63 + "a.example.com", 443, "invalid", 443},
	} {
		if name, port := Recorded(c.authority, c.port); name != c.name || port != c.want {
			t.Errorf("Recorded(%q, %d) = %q, %d, want %q, %d", c.authority, c.port, name, port, c.name, c.want)
		}
	}
}
