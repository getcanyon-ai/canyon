package identity

import (
	"net/http"
	"slices"
	"strings"
)

// ForgedHeader reports whether a request header is one only a proxy may set, which a client sends to pass
// for one, and so never reaches an app (contracts/executor.md, The front door): claims about the connection,
// which frameworks read (Rack takes X-Forwarded-Ssl: on as https, Rails Client-Ip as the address), and a
// proxy's word on who the person is (oauth2-proxy's X-Forwarded-* and X-Auth-Request-*, an AWS load
// balancer's X-Amzn-Oidc-*, Remote-User, and other gateways' that an app moved from behind one may still trust,
// these and no others: forgedPrefixes and forgedNames). Every name with an underscore goes too, as nginx drops them:
// CGI-style stacks (Rack, Django, PHP, WSGI) read X_Forwarded_Email as X-Forwarded-Email. The name is
// compared however it is spelled; a caller deletes it by the key it was sent under, which Header.Del,
// canonicalising, would miss. Both front doors use it, the server's on one host and canyon-identity's.
func ForgedHeader(name string) bool {
	h := http.CanonicalHeaderKey(name)
	if strings.Contains(h, "_") {
		return true
	}
	if slices.Contains(forgedNames, h) {
		return true
	}
	return slices.ContainsFunc(forgedPrefixes, func(p string) bool { return strings.HasPrefix(h, p) })
}

// forgedPrefixes are canonical, as CanonicalHeaderKey spells the name they are compared with.
var forgedPrefixes = []string{
	"X-Forwarded-", "X-Auth-Request-", "X-Amzn-Oidc-", // oauth2-proxy's and nginx's auth_request, and an AWS load balancer's
	"X-Goog-Authenticated-User-", // Google's IAP
	"Cf-Access-",                 // Cloudflare Access
	"X-Ms-Client-Principal",      // Azure App Service's (Easy Auth): the principal itself, and its -Name, -Id and -Idp
	"X-Ms-Token-",                // Azure App Service's (Easy Auth) tokens
	"X-Pomerium-",                // Pomerium
	"X-Authentik-",               // authentik
	"Tailscale-User-",            // Tailscale's serve and funnel
	"X-Vouch-",                   // Vouch Proxy
	// never X-Auth-: X-Auth-Token is many an API's own header
}

var forgedNames = []string{
	"X-Real-Ip", "Client-Ip", "X-Client-Ip", "True-Client-Ip", "Cf-Connecting-Ip", "X-Cluster-Client-Ip", "Forwarded", // the client's address
	"X-Scheme", "X-Url-Scheme", "Front-End-Https", // the scheme
	"Remote-User", "X-Remote-User", "X-Webauth-User", // who the person is
	"Remote-Email", "Remote-Groups", "Remote-Name", // Authelia's, authentik's and Organizr's: by name, never a Remote- prefix
	"X-Goog-Iap-Jwt-Assertion", // Google IAP's signed word on the person
	"Teleport-Jwt-Assertion",   // Teleport's application access
}
