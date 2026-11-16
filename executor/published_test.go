package executor

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const serverID = "3f1c0b9a7e2a9d3c4f1c0b9a7e2a9d3c4f1c0b9a7e2a9d3c4f1c0b9a7e2a9d3c"

// GET /published answers the running canyon-server's Id, and its bindings as docker container inspect reads
// them, those it was created with and those it holds as two lists, each once and sorted, in the contract's
// published_result: a publish on every address is "" as created and 0.0.0.0 and :: as held, and a port
// exposed but published nowhere is none.
func TestPublishedAnswersTheServersBindingsAsDockerHoldsThem(t *testing.T) {
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	inspect := `{"id":"` + serverID + `","running":true,"created":{"7777/tcp":[{"HostIp":"","HostPort":"7777"},{"HostIp":"127.0.0.1","HostPort":"7777"},{"HostIp":"127.0.0.1","HostPort":"7777"}]},` +
		`"held":{"7777/tcp":[{"HostIp":"0.0.0.0","HostPort":"7777"},{"HostIp":"::","HostPort":"7777"},{"HostIp":"127.0.0.1","HostPort":"7777"}],"7780/tcp":null}}`
	must(t, os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+dir+"/argv\necho '"+inspect+"'\n"), 0o700)) //nolint:gosec // test fake
	srv := httptest.NewServer(Handler(&Executor{Docker: docker}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/published")
	must(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /published: %d %s", resp.StatusCode, body)
	}
	validates(t, schemaOf(t, "published_result"), body)
	want := `{"id":"` + serverID + `","created":[{"port":"7777/tcp","host_ip":"","host_port":"7777"},{"port":"7777/tcp","host_ip":"127.0.0.1","host_port":"7777"}],` +
		`"held":[{"port":"7777/tcp","host_ip":"0.0.0.0","host_port":"7777"},{"port":"7777/tcp","host_ip":"127.0.0.1","host_port":"7777"},{"port":"7777/tcp","host_ip":"::","host_port":"7777"}]}` + "\n"
	if string(body) != want {
		t.Errorf("the answer:\n%s\nwant\n%s", body, want)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	if got := strings.Split(strings.TrimSpace(string(argv)), "\n"); got[0] != "container" || got[1] != "inspect" || got[len(got)-1] != "canyon-server" {
		t.Errorf("docker %q, want container inspect of canyon-server", got)
	}

	for name, script := range map[string]string{
		"a daemon that does not answer":                           "echo 'Cannot connect to the Docker daemon' >&2; exit 1",
		"no container canyon-server":                              "echo 'Error: No such container: canyon-server' >&2; exit 1",
		"an inspect it cannot read":                               "echo '{\"created\":'",
		"a canyon-server that is stopped, left from another demo": `echo '{"id":"` + serverID + `","running":false,"created":{"7777/tcp":[{"HostIp":"127.0.0.1","HostPort":"7777"}]},"held":{}}'`,
	} {
		must(t, os.WriteFile(docker, []byte("#!/bin/sh\n"+script+"\n"), 0o700)) //nolint:gosec // test fake
		resp, err := http.Get(srv.URL + "/published")
		must(t, err)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("%s: %d, want 502: the executor cannot tell", name, resp.StatusCode)
		}
	}

	must(t, os.WriteFile(docker, []byte("#!/bin/sh\necho '{\"id\":\""+serverID+"\",\"running\":true,\"created\":null,\"held\":{}}'\n"), 0o700)) //nolint:gosec // test fake
	got, err := (&Client{Base: srv.URL}).Published(t.Context())
	if err != nil || got.ID != serverID || got.Created == nil || len(got.Created) != 0 || got.Held == nil || len(got.Held) != 0 {
		t.Errorf("a server published nowhere: %#v %v, want two empty lists", got, err)
	}
}

// GET /published's answer is taken only in the contract's shape: the fixture's published_result is read as it
// is, and an answer out of shape, or any status but 200 — the Kubernetes executor's or an older one's 404, a
// 502 — is an error, so the server reads no binding from it.
func TestTheClientTakesOnlyAContractShapedPublishedAnswer(t *testing.T) {
	raw, err := os.ReadFile("../contracts/testdata/executor/claims-portal.json")
	must(t, err)
	var fixture struct {
		Published json.RawMessage `json:"published_result"`
	}
	must(t, json.Unmarshal(raw, &fixture))
	loopback := []Binding{{Port: "7777/tcp", HostIP: "127.0.0.1", HostPort: "7777"}}
	answer := func(id, created, held string) string {
		return `{"id":"` + id + `","created":` + created + `,"held":` + held + `}`
	}
	lo := `[{"port":"7777/tcp","host_ip":"127.0.0.1","host_port":"7777"}]`
	for _, tc := range []struct {
		name, answer string
		status       int
		want         *Published
	}{
		{"the fixture", string(fixture.Published), 200, &Published{ID: serverID, Created: loopback, Held: loopback}},
		{"published nowhere", answer(serverID, `[]`, `[]`), 200, &Published{ID: serverID, Created: []Binding{}, Held: []Binding{}}},
		{"every address", answer(serverID, `[{"port":"7777/tcp","host_ip":"","host_port":"7777"}]`, `[]`), 200, &Published{ID: serverID, Created: []Binding{{Port: "7777/tcp", HostPort: "7777"}}, Held: []Binding{}}},
		{"an executor without the call", `{"error":"no such call"}`, 404, nil},
		{"an executor that cannot tell", `{"error":"canyon-server is not running"}`, 502, nil},
		{"the answer before this one", `{"bindings":[]}`, 200, nil},
		{"no id", `{"created":[],"held":[]}`, 200, nil},
		{"an id that is not a container's", answer("canyon-server", lo, lo), 200, nil},
		{"a short id", answer(serverID[:12], lo, lo), 200, nil},
		{"no held", `{"id":"` + serverID + `","created":[]}`, 200, nil},
		{"held null", answer(serverID, `[]`, `null`), 200, nil},
		{"created null", answer(serverID, `null`, `[]`), 200, nil},
		{"a key the contract does not name", `{"id":"` + serverID + `","created":[],"held":[],"running":true}`, 200, nil},
		{"a binding's key the contract does not name", answer(serverID, `[]`, `[{"port":"7777/tcp","host_ip":"127.0.0.1","host_port":"7777","proto":"tcp"}]`), 200, nil},
		{"something after the answer", answer(serverID, `[]`, `[]`) + ` {}`, 200, nil},
		{"a stray brace after the answer", answer(serverID, `[]`, `[]`) + `}`, 200, nil},
		{"a held port without its protocol", answer(serverID, `[]`, `[{"port":"7777","host_ip":"127.0.0.1","host_port":"7777"}]`), 200, nil},
		{"a created port without its protocol", answer(serverID, `[{"port":"7777","host_ip":"127.0.0.1","host_port":"7777"}]`, `[]`), 200, nil},
		{"not JSON", `127.0.0.1:7777`, 200, nil},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/published" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.answer)
		}))
		got, err := (&Client{Base: srv.URL}).Published(t.Context())
		srv.Close()
		if tc.want == nil && (err == nil || !reflect.DeepEqual(got, Published{})) || tc.want != nil && (err != nil || !reflect.DeepEqual(got, *tc.want)) {
			t.Errorf("%s: %#v %v, want %#v", tc.name, got, err, tc.want)
		}
	}
	if got, err := (&Client{Base: "http://127.0.0.1:1"}).Published(t.Context()); err == nil || !reflect.DeepEqual(got, Published{}) {
		t.Errorf("an executor that is not up: %#v %v, want an error", got, err)
	}
}
