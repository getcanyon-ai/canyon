package identity

import "testing"

// Every header a client could pass for a proxy with is forged, however it is spelled; the request's own
// headers, and the ones a front door sets after the strip, are not on the list.
func TestForgedHeader(t *testing.T) {
	for _, name := range []string{"X-Forwarded-Email", "x-FORWARDED-email", "X-Forwarded-For", "X-Forwarded-Ssl", "X-Auth-Request-Groups", "X-Amzn-Oidc-Data",
		"X_Forwarded_Email", "x_FORWARDED_user", "X-Forwarded_Groups", "Remote-User", "X-Remote-User", "X-Webauth-User", "X-Real-IP", "Client-IP",
		"X-Client-IP", "True-Client-IP", "Cf-Connecting-IP", "x-cluster-CLIENT-ip", "Forwarded", "X-Scheme", "X-Url-Scheme", "Front-End-Https"} {
		if !ForgedHeader(name) {
			t.Errorf("%s is not forged", name)
		}
	}
	for _, name := range []string{"Authorization", "Cookie", "Accept", "Content-Type", "X-Request-Id", "User-Agent", "Forwarded-For", "X-Forwarded",
		"X-Goog-Authenticated-User", "Cf-Access", "X-Ms-Client", "X-Pomerium", "X-Goog-Api-Key", "Cf-Ray",
		"X-Auth-Token", "X-Authentik", "Tailscale-User", "X-Ms-Token", "Remote-Port", "X-Goog-Iap", "X-Vouch", "Teleport-Jwt"} { // X-Auth-Token is an API's own: no X-Auth- prefix, no Remote- prefix
		if ForgedHeader(name) {
			t.Errorf("%s is forged", name)
		}
	}
}

// gatewayHeaders are other gateways' word on who the person is, each prefix's headers as the gateway sends them and
// spelled otherwise (#2460): Google IAP's, Cloudflare Access's, Azure App Service's (Easy Auth) and Pomerium's. An app
// moved from behind one may still trust them; only Canyon's identity token says who the person is.
var gatewayHeaders = map[string][]string{
	"X-Goog-Authenticated-User-":   {"X-Goog-Authenticated-User-Email", "X-Goog-Authenticated-User-Id", "x-goog-authenticated-user-EMAIL"},
	"Cf-Access-":                   {"Cf-Access-Authenticated-User-Email", "Cf-Access-Jwt-Assertion", "CF-ACCESS-JWT-ASSERTION", "cf-access-client-id"},
	"X-Ms-Client-Principal":        {"X-Ms-Client-Principal", "X-Ms-Client-Principal-Name", "X-MS-CLIENT-PRINCIPAL-ID", "x-ms-client-principal-idp"},
	"X-Pomerium-":                  {"X-Pomerium-Jwt-Assertion", "X-Pomerium-Claim-Email", "X-POMERIUM-CLAIM-GROUPS", "x-pomerium-authenticated-user-email"},
	"X-Vouch-":                     {"X-Vouch-User", "X-Vouch-Idp-Claims-Email", "x-vouch-success"},
	"Teleport-Jwt-Assertion":       {"Teleport-Jwt-Assertion", "teleport-jwt-assertion", "TELEPORT-JWT-ASSERTION"},
	"X-Goog-Iap-Jwt-Assertion":     {"X-Goog-Iap-Jwt-Assertion", "x-goog-iap-jwt-assertion", "X-GOOG-IAP-JWT-ASSERTION"},
	"X-Ms-Token-":                  {"X-Ms-Token-Aad-Id-Token", "X-Ms-Token-Aad-Access-Token", "x-ms-token-github-access-token"},
	"X-Authentik-":                 {"X-Authentik-Username", "X-Authentik-Email", "X-AUTHENTIK-GROUPS", "x-authentik-jwt"},
	"Tailscale-User-":              {"Tailscale-User-Login", "Tailscale-User-Name", "TAILSCALE-USER-PROFILE-PIC"},
	"Remote-Email, -Groups, -Name": {"Remote-Email", "Remote-Groups", "Remote-Name", "REMOTE-EMAIL", "remote-groups"},
}

// Each other gateway's identity headers are forged, by its prefix, however they are spelled (#2460).
func TestOtherGatewaysIdentityHeadersAreForged(t *testing.T) {
	for prefix, names := range gatewayHeaders {
		for _, name := range names {
			if !ForgedHeader(name) {
				t.Errorf("%s (%s*) is not forged", name, prefix)
			}
		}
	}
}
