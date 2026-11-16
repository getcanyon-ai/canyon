// Package place checks the places a hand-off release names (design/HAND-OFF.md §4): an app's address and a
// resource's endpoint, parsed with net/url and printed back exactly as sent, never a credential. The report
// route checks a platform's report with it, and the deploy repo's renderer what a template annotates.
package place

import (
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// EndpointSchemes is each resource type's endpoint schemes.
var EndpointSchemes = map[string][]string{"postgres": {"postgres", "postgresql"}, "s3": {"s3", "https"}}

var (
	hostRe         = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`) // lower-case labels, one or more: a cluster's service name too
	addressPathRe  = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)*/?$`)                                                    // segments of unreserved characters, a trailing slash or none
	endpointPathRe = regexp.MustCompile(`^(/[a-z0-9_-][a-z0-9._-]{0,62})?$`)                                           // the database or the bucket, or nothing
	digitsRe       = regexp.MustCompile(`^[0-9]+$`)
	portRe         = regexp.MustCompile(`^[1-9][0-9]{0,4}$`) // no leading zero, as the schema's
)

// strictURL is an endpoint's or an address's parse (contracts/routes.md, the report row), with net/url and
// never a pattern over the whole: printable ASCII, at most limit bytes, none of % \ @ ? #, a scheme of schemes,
// no userinfo, opaque part, query (an empty ? too) or fragment, a lower-case host that is a domain or an IP
// address — a name whose last label is all digits is an address spelled another way (2130706433, 0x7f.1) and
// refused — a port of 1-65535 with no leading zero when there is one, a path path takes with no . or .. segment,
// and the URL printing back exactly as sent.
func strictURL(s string, limit int, schemes []string, path *regexp.Regexp) bool {
	if s == "" || len(s) > limit || strings.ContainsAny(s, `%\@?#`) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil || !slices.Contains(schemes, u.Scheme) || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != s {
		return false
	}
	host := u.Hostname()
	if u.Host != strings.ToLower(u.Host) { // an IPv6 address too: [fd00::7], never [FD00::7]
		return false
	}
	if _, err := netip.ParseAddr(host); err != nil {
		labels := strings.Split(host, ".")
		if len(host) > 253 || !hostRe.MatchString(host) || digitsRe.MatchString(labels[len(labels)-1]) {
			return false
		}
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || !portRe.MatchString(port) || n > 65535 {
			return false
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return false
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment != "" && strings.Trim(segment, ".") == "" { // ., .. and dots alone: the schema's rule
			return false
		}
	}
	return path.MatchString(u.Path)
}

// ValidAddress is whether s is an app's address: https, a lower-case host, an optional port, a path of
// unreserved segments.
func ValidAddress(s string) bool { return strictURL(s, 2048, []string{"https"}, addressPathRe) }

// ValidEndpoint is whether s is where a resource of typ is: its type's scheme, a lower-case host, an
// optional port, at most one path segment.
func ValidEndpoint(s, typ string) bool {
	return strictURL(s, 255, EndpointSchemes[typ], endpointPathRe)
}
