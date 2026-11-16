// Package destination canonicalises what an app calls out to (design/EGRESS.md §3.3): one function the
// egress check, the routes and the records share, so a destination is one name wherever it is compared.
package destination

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

// Why a destination is refused. The check answers each with invalid_destination but ErrPort, which is
// port (contracts/routes.md, Egress control, The check); a route answers each 400 invalid_destination.
var (
	ErrUserinfo    = errors.New("destination: userinfo")
	ErrIPLiteral   = errors.New("destination: an IP literal")
	ErrSingleLabel = errors.New("destination: a single label")
	ErrLength      = errors.New("destination: a label over 63 characters or a name over 253")
	ErrName        = errors.New("destination: not a host name")
	ErrPort        = errors.New("destination: a port other than 80 or 443")
	ErrMapped      = errors.New("destination: not the name it canonicalises to")
)

// Canonical is the name and port an authority names — a CONNECT's, or an absolute-form URL's, which
// takes port, its scheme's, when it names none: lower-case; one trailing dot dropped; IDNA (UTS 46, the
// lookup profile) to A-labels; userinfo refused; every IP-literal encoding refused; single-label names
// refused; labels ≤63, names ≤253; ports 80 and 443 only. UTS 46 maps or drops some characters — a
// zero-width space, fullwidth letters, 。 and ① each come back as another ASCII name — so a host that is
// not its own name once lower-cased in ASCII, one trailing dot dropped, is refused (ErrMapped): Envoy
// dials the host it was given, so the check never answers for a name other than the one Envoy dials.
func Canonical(authority string, port int) (string, int, error) {
	if strings.Contains(authority, "@") {
		return "", 0, ErrUserinfo
	}
	// RFC 3986: brackets in an authority hold an IP literal and nothing else
	if strings.HasPrefix(authority, "[") {
		return "", 0, ErrIPLiteral
	}
	host, p := authority, strconv.Itoa(port)
	if _, err := netip.ParseAddr(authority); err != nil {
		if i := strings.LastIndexByte(authority, ':'); i >= 0 {
			host, p = authority[:i], authority[i+1:]
		}
	}
	n, err := name(host)
	if err != nil {
		return "", 0, err
	}
	if n != strings.TrimSuffix(asciiLower(host), ".") {
		return "", 0, ErrMapped
	}
	if p != "80" && p != "443" {
		return "", 0, ErrPort
	}
	port, _ = strconv.Atoi(p)
	return n, port, nil
}

// Recorded is what a record keeps of a call to authority, port the scheme's when authority names none
// (contracts/routes.md, the receiver): the name as Canonical keeps it and the port called, whatever it is; a
// dotted IPv4 address as it is, which layer 1's records keep; anything else invalid, on 443.
func Recorded(authority string, port int) (string, int) {
	if strings.HasPrefix(authority, "[") {
		return "invalid", 443
	}
	host := authority
	if h, p, err := net.SplitHostPort(authority); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p { // 0443 and +443 are the check's port refusals, never 443's row
			return "invalid", 443
		}
		host, port = h, n
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.Is4() {
		return ip.String(), port
	}
	if n, _, err := Canonical(host, 443); err == nil {
		return n, port
	}
	return "invalid", 443
}

// Entry is an allowlist entry canonicalised: a name, or *. and a name. A person types it, so a U-label
// is mapped to its A-label, as a route takes it (bücher.de is xn--bcher-kva.de).
func Entry(s string) (string, error) {
	rest, wild := strings.CutPrefix(s, "*.")
	n, err := name(rest)
	if err != nil {
		return "", err
	}
	if wild {
		return "*." + n, nil
	}
	return n, nil
}

// Allows reports whether an entry allows a name: a name allows itself alone, and *.stripe.com any name
// any depth below stripe.com, never stripe.com. It compares strings, so it is right only on Entry's and
// Canonical's output: an entry or a name neither made is refused nowhere here.
func Allows(entry, name string) bool {
	if base, wild := strings.CutPrefix(entry, "*."); wild {
		return strings.HasSuffix(name, "."+base)
	}
	return name == entry
}

func name(host string) (string, error) {
	if _, err := netip.ParseAddr(host); err == nil {
		return "", ErrIPLiteral
	}
	// UTS 46's mapping folds case itself; a Unicode lower-casing first would fold İ to i, not to i̇
	n, err := idna.Lookup.ToASCII(strings.TrimSuffix(host, "."))
	if err != nil {
		return "", ErrName
	}
	labels := strings.Split(n, ".")
	for _, l := range labels {
		if l == "" {
			return "", ErrName
		}
	}
	if number(labels[len(labels)-1]) {
		return "", ErrIPLiteral
	}
	if len(labels) < 2 {
		return "", ErrSingleLabel
	}
	if len(n) > 253 {
		return "", ErrLength
	}
	for _, l := range labels {
		if len(l) > 63 {
			return "", ErrLength
		}
	}
	return n, nil
}

// number is the WHATWG URL Standard's "ends in a number" test of a host's last label: decimal digits, or
// 0x and hex digits. A client parses such a host as an IPv4 address in any of its encodings — dotted,
// integer, hex, octal — and no top-level domain is one.
func number(label string) bool {
	if hex, ok := strings.CutPrefix(label, "0x"); ok {
		return strings.Trim(hex, "0123456789abcdef") == ""
	}
	return strings.Trim(label, "0123456789") == ""
}

// asciiLower lower-cases A to Z alone: Unicode's lower-casing turns the Kelvin sign into k.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}
