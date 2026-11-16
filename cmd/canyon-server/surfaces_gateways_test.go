package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Other gateways' word on who the person is never reaches an app on one host, by each one's prefix, however a client
// spells it (#2460): Google IAP's, Cloudflare Access's, Azure App Service's and Pomerium's, as the sidecar strips them.
// An app moved from behind one may still trust them; only Canyon's identity token says who the person is.
func TestOtherGatewaysIdentityHeadersNeverReachAnApp(t *testing.T) {
	for prefix, sent := range map[string][]string{
		"X-Goog-Authenticated-User-":   {"X-Goog-Authenticated-User-Email", "X-Goog-Authenticated-User-Id", "x-goog-authenticated-user-EMAIL"},
		"Cf-Access-":                   {"Cf-Access-Authenticated-User-Email", "Cf-Access-Jwt-Assertion", "CF-ACCESS-JWT-ASSERTION"},
		"X-Ms-Client-Principal":        {"X-Ms-Client-Principal", "X-Ms-Client-Principal-Name", "X-MS-CLIENT-PRINCIPAL-ID", "x-ms-client-principal-idp"},
		"X-Pomerium-":                  {"X-Pomerium-Jwt-Assertion", "X-Pomerium-Claim-Email", "X-POMERIUM-CLAIM-GROUPS"},
		"X-Vouch-":                     {"X-Vouch-User", "X-Vouch-Idp-Claims-Email", "x-vouch-success"},
		"Teleport-Jwt-Assertion":       {"Teleport-Jwt-Assertion", "teleport-jwt-assertion", "TELEPORT-JWT-ASSERTION"},
		"X-Goog-Iap-Jwt-Assertion":     {"X-Goog-Iap-Jwt-Assertion", "x-goog-iap-jwt-assertion", "X-GOOG-IAP-JWT-ASSERTION"},
		"X-Ms-Token-":                  {"X-Ms-Token-Aad-Id-Token", "X-Ms-Token-Aad-Access-Token", "x-ms-token-github-access-token"},
		"X-Authentik-":                 {"X-Authentik-Username", "X-Authentik-Email", "X-AUTHENTIK-GROUPS", "x-authentik-jwt"},
		"Tailscale-User-":              {"Tailscale-User-Login", "Tailscale-User-Name", "TAILSCALE-USER-PROFILE-PIC"},
		"Remote-Email, -Groups, -Name": {"Remote-Email", "Remote-Groups", "Remote-Name", "REMOTE-EMAIL", "remote-groups"},
	} {
		t.Run(prefix, func(t *testing.T) {
			var got http.Header
			upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
			defer upstream.Close()
			transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
			}}
			h := (&app{public: "https://canyon.example.com"}).surfaces(http.NotFoundHandler(), transport)
			req := httptest.NewRequest(http.MethodGet, "http://claims-portal.preview.canyon.example.com/", nil)
			for _, name := range sent {
				req.Header[name] = []string{"dana@meridian.example"} // as the client wrote it, never canonicalised
			}
			req = req.WithContext(context.WithValue(req.Context(), tokenKey{}, "canyons-own")) // as admit leaves it
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got == nil {
				t.Fatal("the app was never reached")
			}
			for name, v := range got {
				if len(v) > 0 && v[0] == "dana@meridian.example" {
					t.Errorf("the app was handed the client's %s: %q", name, v)
				}
			}
			if got.Get("Authorization") != "Bearer canyons-own" {
				t.Errorf("the app's Authorization: %q", got.Get("Authorization"))
			}
		})
	}
}
