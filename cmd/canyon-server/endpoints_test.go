package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// An endpoint is where a resource is and never how to sign in to it (contracts/routes.md, the report row,
// #2432): each refusal below is its own rule, and each acceptance a form a platform sends.
func TestAnEndpointIsAPlaceNeverACredential(t *testing.T) {
	for _, c := range []struct {
		endpoint, typ string
		ok            bool
	}{
		{"postgres://pg.data.meridian.example:5432/claims_portal_preview_db", "postgres", true},
		{"postgresql://pg.data.meridian.example/claims", "postgres", true},
		{"postgres://10.0.4.7:5432/claims", "postgres", true},
		{"postgres://[fd00::7]:5432/claims", "postgres", true},
		{"postgres://pg.data.meridian.example", "postgres", true},
		{"postgres://prod.db.internal/claims", "postgres", true}, // a first label the front door's domain refuses
		{"s3://claims-portal-files", "s3", true},                 // a bucket, one label
		{"s3://claims.files.meridian", "s3", true},
		{"https://claims-files.s3.eu-central-1.amazonaws.com", "s3", true},
		{"https://s3.eu-central-1.amazonaws.com/claims-files", "s3", true},
		{"postgres://app:hunter2@pg.example/claims", "postgres", false}, // userinfo, with a password
		{"postgres://app@pg.example/claims", "postgres", false},         // userinfo alone
		{"postgres://pg.example/claims?sslmode=require", "postgres", false},
		{"postgres://pg.example/claims?", "postgres", false}, // an empty query
		{"postgres://pg.example/claims#x", "postgres", false},
		{"postgres://pg.example/cl%61ims", "postgres", false},
		{`postgres://pg.example\claims`, "postgres", false},
		{"postgres://pg.example/claims/extra", "postgres", false}, // a second path segment
		{"postgres://pg.example/Claims", "postgres", false},       // the path's characters
		{"postgres://pg.example/", "postgres", false},             // a bare slash: an endpoint's path is empty or a segment
		{"s3://claims-files", "postgres", false},                  // a scheme another type's
		{"postgres://pg.example/claims", "s3", false},
		{"mysql://pg.example/claims", "postgres", false},
		{"postgres://pg.example/claims", "redis", false},  // a type off the menu takes nothing
		{"postgres:pg.example/claims", "postgres", false}, // opaque
		{"postgres://pg.example:0/claims", "postgres", false},
		{"postgres://pg.example:65536/claims", "postgres", false},
		{"postgres://pg.example:/claims", "postgres", false}, // an empty port
		{"postgres://PG.example/claims", "postgres", false},
		{"postgres://pg_example/claims", "postgres", false},
		{"postgres://claims-db-rw:5432/app", "postgres", true}, // one label: a cluster's service name
		{"https://ab", "s3", true},
		{"postgres://[FD00::7]:5432/claims", "postgres", false},    // hosts are lower case, an IPv6 address's too
		{"postgres://pg.example:005432/claims", "postgres", false}, // a port with a leading zero
		{"postgres://pg.example:543210/claims", "postgres", false},
		{"postgres://pg.example/..", "postgres", false}, // a dot segment
		{"postgres://pg.example/.", "postgres", false},
		{"postgres://2130706433/claims", "postgres", false}, // an address spelled as a name
		{"postgres://pg.example/claims.v2", "postgres", true},
		{"postgres://10.0.04.7/claims", "postgres", false}, // the schema's parity cases (contracts/validate.py)
		{"postgres://pg.1-2/claims", "postgres", true},
		{"postgres://pg.1a/claims", "postgres", true},
		{"postgres://pg.example:65535/claims", "postgres", true},
		{"postgres://pg.example:65536/claims", "postgres", false},
		{"postgres://" + strings.Repeat("a", 63) + ".example/claims", "postgres", true}, // a label of 63, the most
		{"postgres://" + strings.Repeat("a", 64) + ".example/claims", "postgres", false},
		{"postgres://pg." + strings.Repeat("a", 64) + "/claims", "postgres", false},
		{"postgres://pg.example/clai ms", "postgres", false},
		{"postgres://pg.example/claimsé", "postgres", false},
		{"postgres://" + strings.Repeat("a", 240) + ".example/claims", "postgres", false}, // over 255 bytes
		{"", "postgres", false},
	} {
		if got := validEndpoint(c.endpoint, c.typ); got != c.ok {
			t.Errorf("%s endpoint %q: accepted %v, want %v", c.typ, c.endpoint, got, c.ok)
		}
	}
}

// The report's address is parsed the same way, https alone, its path any segments of unreserved characters
// (#2432): it took https://h/?token=x before.
func TestAnAddressIsHTTPSWithNoCredentialOrQuery(t *testing.T) {
	for address, ok := range map[string]bool{
		"https://claims.apps.meridian.example":           true,
		"https://claims.apps.meridian.example/":          true,
		"https://apps.meridian.example/claims":           true,
		"https://claims.apps.meridian.example:8443":      true,
		"https://10.0.4.7":                               true,
		"https://claims.apps.meridian.example/?token=x":  false,
		"https://claims.apps.meridian.example/?":         false,
		"https://u:p@claims.apps.meridian.example":       false,
		"https://claims.apps.meridian.example/#frag":     false,
		"https://claims.apps.meridian.example/a/b":       true,
		"https://apps.meridian.example/Claims/v2.1~x/":   true,
		"https://claims":                                 true, // one label
		"https://[fd00::7]:8443/claims":                  true,
		"https://[FD00::7]/claims":                       false,
		"https://Claims.apps.meridian.example":           false,
		"https://claims.apps.meridian.example//a":        false,
		"https://claims.apps.meridian.example/a b":       false,
		"https://claims.apps.meridian.example:000443/":   false,
		"https://claims.apps.meridian.example/a/../b":    false,
		"https://claims.apps.meridian.example/./a":       false,
		"https://claims.apps.meridian.example/..":        false,
		"https://2130706433/":                            false,
		"https://0x7f.1/":                                false,
		"https://apps.meridian.example/v2.1/.well-known": true,
		`https://claims.apps.meridian.example\a`:         false,
		"http://claims.apps.meridian.example":            false,
		"https://claims.apps.meridian.example/%2e":       false,
		"s3://claims-files":                              false,
	} {
		if got := validAddress(address); got != ok {
			t.Errorf("address %q: accepted %v, want %v", address, got, ok)
		}
	}
}

// A deployed report's resources name the release's own resources, each once: so never more than it has.
func TestEndpointsFitTheReleasesResources(t *testing.T) {
	declared := map[string]string{"db": "postgres", "files": "s3"}
	db := reportedResource{Name: "db", Endpoint: "postgres://pg.example/claims"}
	files := reportedResource{Name: "files", Endpoint: "s3://claims-files"}
	for name, c := range map[string]struct {
		reported []reportedResource
		ok       bool
	}{
		"none":                {nil, true},
		"one":                 {[]reportedResource{db}, true},
		"both":                {[]reportedResource{db, files}, true},
		"an unknown name":     {[]reportedResource{{Name: "cache", Endpoint: "postgres://pg.example/x"}}, false},
		"a name twice":        {[]reportedResource{db, db}, false},
		"more than it has":    {[]reportedResource{db, files, db}, false},
		"the other's scheme":  {[]reportedResource{{Name: "db", Endpoint: "s3://claims-files"}}, false},
		"a credential in one": {[]reportedResource{db, {Name: "files", Endpoint: "s3://key:secret@claims-files"}}, false},
	} {
		if got := endpointsFit(c.reported, declared); got != c.ok {
			t.Errorf("%s: fits %v, want %v", name, got, c.ok)
		}
	}
	if endpointsFit([]reportedResource{db}, map[string]string{}) {
		t.Error("a release whose package declares nothing took an endpoint")
	}
}

// parseReport takes a deployed report with or without resources, each entry exactly {name, endpoint}, and
// resources on no other status.
func TestParseReportTakesResourcesOnADeployedReportAlone(t *testing.T) {
	const head = `{"status":"deployed","address":"https://claims.apps.meridian.example","digest":"sha256:` + reportDigest + `"`
	for body, ok := range map[string]bool{
		head + `}`:                true,
		head + `,"resources":[]}`: true,
		head + `,"resources":[{"name":"db","endpoint":"postgres://pg.example/x"}]}`: true,
		head + `,"resources":null}`:                                          false,
		head + `,"resources":{}}`:                                            false,
		head + `,"resources":[{"name":"db"}]}`:                               false,
		head + `,"resources":[{"name":"db","endpoint":"x","password":"y"}]}`: false,
		head + `,"resources":[{"name":"db","endpoint":7}]}`:                  false,
		`{"status":"failed","reason":"x","resources":[]}`:                    false,
		`{"status":"removed","resources":[]}`:                                false,
		`{"status":"deployed","address":"https://h.example/?token=x","digest":"sha256:` + reportDigest + `"}`: false,
	} {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			t.Fatal(err)
		}
		if _, got := parseReport(raw); got != ok {
			t.Errorf("%s: shaped %v, want %v", body, got, ok)
		}
	}
}
