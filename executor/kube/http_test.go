package kube

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
)

// The Kubernetes executor serves executor.Handler's calls and the four of its own: POST /sidecars is 200 with
// what it rolled; POST /callers is 200 with
// the pods that hold the addresses asked and 400 on a body out of contract; PUT /access is 204, 404
// with nothing running, 409 {"error":"stale"} for an older file and 400 for a key the schema does not
// name, in the request or in its access file; POST /probe is 200 with the probe's answer, 400 on a form front_door refuses and 502 when the
// probe could not tell.
func TestTheKubernetesCallsAnswerAsTheContractSays(t *testing.T) {
	serve := func(e *Executor) func(method, path, body string) (int, string) {
		srv := httptest.NewServer(Handler(e))
		t.Cleanup(srv.Close)
		return func(method, path, body string) (int, string) {
			return send(t, method, srv.URL+path, body)
		}
	}
	e, req, _ := fixture(t)
	cluster(t, e, healthy)
	call := serve(e)
	access := func(updated string) string {
		return `{"app":"claims-portal","target":"preview","access":` + strings.Replace(string(req.Access), "2026-09-25T10:00:00Z", updated, 1) + `}`
	}
	if code, _ := call("PUT", "/access", access("2026-09-25T10:00:05Z")); code != 404 {
		t.Fatalf("nothing running: %d", code)
	}
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	badKeep := req
	badKeep.Keep = executor.Digests{"pkg_1-a"} // a tag, never a digest
	badRun, err := json.Marshal(badKeep)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body string
		code               int
		answer             string
	}{
		{"PUT", "/access", access("2026-09-25T10:00:05Z"), 204, ""},
		{"PUT", "/access", access("2026-09-25T10:00:04Z"), 409, `{"error":"stale"}`},
		{"PUT", "/access", `{"app":"claims-portal","target":"preview","access":{},"extra":1}`, 400, ""},
		{"PUT", "/access", strings.Replace(access("2026-09-25T10:00:06Z"), `"groups_claim"`, `"extra":1,"groups_claim"`, 1), 400, ""},
		{"POST", "/probe", `{"domain":"canyon.meridian.example","form":"env.app.domain"}`, 400, ""},
		{"GET", "/running?app=claims-portal&target=preview", "", 200, ""},
		{"POST", "/callers", `{"addresses":["10.0.4.7"]}`, 200, `{"callers":[]}`},
		{"POST", "/callers", `{"addresses":[]}`, 400, ""},
		{"POST", "/sidecars", "", 200, `{"image":"` + identityImage + `","rolled":[],"failed":0}`},
		{"POST", "/run", string(badRun), 400, `{"error":"bad request: keep[0] is not a digest: sha256: and 64 lowercase hex digits"}`},
	} {
		if code, answer := call(tc.method, tc.path, tc.body); code != tc.code || tc.answer != "" && answer != tc.answer {
			t.Errorf("%s %s %s: %d %s", tc.method, tc.path, tc.body, code, answer)
		}
	}
	e, _, _ = fixture(t)
	probeCluster(t, e, enforcedLog, true)
	certServer(t, e, "*.preview."+probeDomain, "*.prod."+probeDomain)
	probe := `{"domain":"` + probeDomain + `","form":"app.env.domain"}`
	if code, answer := serve(e)("POST", "/probe", probe); code != 200 || answer != `{"network_policy":"enforced","certificate":"ok","fix":"","warning":""}` {
		t.Errorf("POST /probe: %d %s", code, answer)
	}
	e, _, _ = fixture(t)
	probeCluster(t, e, "unreached\n", true)
	certServer(t, e, "*.preview."+probeDomain, "*.prod."+probeDomain)
	e.probe.deadline = 100 * time.Millisecond
	if code, _ := serve(e)("POST", "/probe", probe); code != 502 {
		t.Errorf("a probe that could not tell: %d", code)
	}
}

// send is the status and body of one call.
func send(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	r, _ := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, strings.TrimSpace(string(b))
}
