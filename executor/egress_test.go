package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var egressReq = func() RunRequest {
	req := runReq
	req.Identity = Identity{Issuer: "http://localhost:7777", Audience: "claims-portal", JWKSURL: "http://canyon-server:7780/.well-known/jwks.json"}
	req.Run, req.Egress = "gr_6f1c0b9a7e2a9d3c", &Egress{Proxy: "http://canyon-egress:3128", NoProxy: []string{"app-postgres"}}
	return req
}()

// dockerNets wraps fakes' docker: network inspect of canyon-app-claims-portal answers net when it is
// not "", else that the network does not exist; network ls and an inspect of all it lists answer the
// daemon's other networks, <dir>/others (none unless written); the door check — the helper's nc to port
// 3128 — exits 1 when door is false.
func dockerNets(t *testing.T, dir, net string, door bool) {
	t.Helper()
	must(t, os.Rename(filepath.Join(dir, "docker"), filepath.Join(dir, "docker-base")))
	if net != "" {
		must(t, os.WriteFile(filepath.Join(dir, "net"), []byte(net), 0o600))
	}
	script := `#!/bin/sh
d="` + dir + `"
if [ "$1 $2" = "network ls" ]; then [ -e "$d/others" ] && echo others; exit 0; fi
if [ "$1 $2 $3" = "network inspect others" ]; then exec cat "$d/others"; fi
if [ "$1 $2" = "network inspect" ]; then printf '%s\n' "$@" >> "$d/argv"; [ -e "$d/net" ] && exec cat "$d/net"; exit 1; fi
eval "last=\${$#}"
if [ "$1 $2" = "run --rm" ] && [ "$last" = 3128 ] && [ -n "` + map[bool]string{true: "", false: "closed"}[door] + `" ]; then printf '%s\n' "$@" >> "$d/argv"; exit 1; fi
exec "$d/docker-base" "$@"
`
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700)) //nolint:gosec // test fake
}

// netJSON is docker network inspect's {{json .}} of canyon-app-claims-portal on 172.28.3.0/24: each
// member by name at its address.
func netJSON(internal bool, members map[string]string) string {
	containers := map[string]endpoint{}
	for name, addr := range members {
		containers["id-"+name] = endpoint{Name: name, IPv4Address: addr + "/24"}
	}
	n := appNet{Name: "canyon-app-claims-portal", Internal: internal, Containers: containers}
	n.IPAM.Config = []ipamConfig{{Subnet: "172.28.3.0/24", Gateway: "172.28.3.1"}}
	b, _ := json.Marshal(n)
	return string(b)
}

// design/EGRESS.md §2.2–2.3: a network made for a /run carrying egress is --internal, its gateway isolated
// (no address of the host's on it), docker handing out its upper /25 alone, and shared with the door,
// canyon-egress; on it the container starts at its target's fixed address in the lower /25 — preview .10
// or .12, prod .11 or .13, whichever the container a redeploy sets aside does not hold — and carries its
// run as canyon.gate_run. Each is 409 before anything is started: a /run without egress on an internal
// network, and one with egress on a network that is not (contracts/executor.md, Egress).
func TestRunPinsTargetAddresses(t *testing.T) {
	for _, tc := range []struct {
		name, target, net, want string
	}{
		{"a first deploy makes the network", "preview", "", "172.28.0.10"},
		{"preview's first", "preview", netJSON(true, map[string]string{"canyon-server": "172.28.3.130"}), "172.28.3.10"},
		{"preview's redeploy", "preview", netJSON(true, map[string]string{"claims-portal-preview": "172.28.3.10"}), "172.28.3.12"},
		{"preview's next", "preview", netJSON(true, map[string]string{"claims-portal-preview": "172.28.3.12"}), "172.28.3.10"},
		{"a crash left one aside", "preview", netJSON(true, map[string]string{"claims-portal-preview-previous": "172.28.3.10"}), "172.28.3.12"},
		{"prod's first", "prod", netJSON(true, map[string]string{"claims-portal-preview": "172.28.3.10"}), "172.28.3.11"},
		{"prod's redeploy", "prod", netJSON(true, map[string]string{"claims-portal-prod": "172.28.3.11"}), "172.28.3.13"},
		{"the pool's first /24 held by another network", "preview", "", "172.28.1.10"},
	} {
		e, dir := fakes(t)
		dockerNets(t, dir, tc.net, true)
		if strings.HasPrefix(tc.name, "the pool's first") { // docker refuses no overlap once --ip-range is given: the executor skips it itself
			must(t, os.WriteFile(filepath.Join(dir, "others"), []byte(`[{"Name":"ux-other","IPAM":{"Config":[{"Subnet":"172.28.0.0/24"}]}}]`), 0o600))
		}
		req := egressReq
		req.Target = tc.target
		if _, err := e.Run(context.Background(), req); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		argv := read(t, dir, "argv")
		ran := argv[strings.Index(argv, "run\n-d\n"):]
		ran = ran[:strings.Index(ran, "sha256:")]
		pinned := "--ip\n" + tc.want + "\n"
		if !strings.Contains(ran, pinned) || !strings.Contains(ran, "--label\ncanyon.gate_run=gr_6f1c0b9a7e2a9d3c\n") {
			t.Errorf("%s: docker run %q; want the address %q and the run's label", tc.name, ran, tc.want)
		}
		if !strings.Contains(argv, "network\nconnect\n--gw-priority=-1\ncanyon-app-claims-portal\ncanyon-egress\n") {
			t.Errorf("%s: the door was not joined to the internal network:\n%s", tc.name, argv)
		}
		sub := strings.TrimSuffix(tc.want, "10")
		if tc.net == "" && (!strings.Contains(argv, "network\ncreate\n--ipv6=false\n--subnet\n"+sub+"0/24\n--gateway\n"+sub+"1\n--internal\n-o\ncom.docker.network.bridge.gateway_mode_ipv4=isolated\n--ip-range\n"+sub+"128/25\n--label\ncanyon.app=claims-portal\ncanyon-app-claims-portal\n") || strings.Count(argv, "network\ncreate\n") != 1) {
			t.Errorf("%s: no internal network, its gateway isolated, its upper /25 for docker:\n%s", tc.name, argv)
		}
	}
	for _, tc := range []struct {
		name, net, want string
		req             RunRequest
	}{
		{"no egress on an internal network", netJSON(true, nil), "canyon-app-claims-portal is internal", runReq},
		{"egress on an open network", netJSON(false, map[string]string{"claims-portal-preview": "172.28.3.4"}), "canyon-app-claims-portal is not internal", egressReq},
	} {
		e, dir := fakes(t)
		dockerNets(t, dir, tc.net, true)
		srv := httptest.NewServer(Handler(e))
		_, err := (&Client{Base: srv.URL}).Run(context.Background(), tc.req)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), tc.want) || strings.Contains(read(t, dir, "argv"), "run\n-d\n") {
			t.Errorf("%s: %v; want 409 naming it, nothing started\n%s", tc.name, err, read(t, dir, "argv"))
		}
	}
}

// The door is on internal networks alone: a /run without egress makes an open network shared with the
// joined containers and never with canyon-egress, so a holder that is down breaks no deploy routed past it.
func TestAnOpenNetworkNeverJoinsTheDoor(t *testing.T) {
	e, dir := fakes(t)
	dockerNets(t, dir, "", true)
	_, err := e.Run(context.Background(), runReq)
	must(t, err)
	if argv := read(t, dir, "argv"); strings.Contains(argv, "canyon-egress") || strings.Contains(argv, "--internal") || !strings.Contains(argv, "network\nconnect\n--gw-priority=-1\ncanyon-app-claims-portal\napp-postgres\n") {
		t.Fatalf("a /run without egress: the door joined, or the network internal:\n%s", argv)
	}
}

// design/EGRESS.md §2.2, §4.4: NO_PROXY is every in-network host the executor or the server wrote — the
// database, the key set's host, the loopback — each once, so token verification never reaches Envoy; the
// proxy variables in both cases, NODE_USE_ENV_PROXY, and the JVM's properties after the app's own
// JDK_JAVA_OPTIONS. Without egress none of them is set; with it, env naming one is 400.
func TestNoProxyCoversEveryInNetworkURL(t *testing.T) {
	e, dir := fakes(t)
	req := egressReq
	req.Egress = &Egress{Proxy: "http://canyon-egress:3128", NoProxy: []string{"app-postgres", "localhost"}}
	req.Env = map[string]string{"JDK_JAVA_OPTIONS": "-Xmx512m"}
	_, err := e.Run(context.Background(), req)
	must(t, err)
	env := slices.DeleteFunc(strings.Split(read(t, dir, "run-env"), "\n"), func(kv string) bool { // the variables egress sets, nothing of the test's own
		name, _, _ := strings.Cut(kv, "=")
		return !slices.Contains(proxyVars, name) && name != "JDK_JAVA_OPTIONS"
	})
	const direct = "app-postgres,localhost,canyon-server,127.0.0.1"
	for _, want := range []string{
		"HTTPS_PROXY=http://canyon-egress:3128", "HTTP_PROXY=http://canyon-egress:3128", "https_proxy=http://canyon-egress:3128", "http_proxy=http://canyon-egress:3128",
		"NO_PROXY=" + direct, "no_proxy=" + direct, "NODE_USE_ENV_PROXY=1",
		"JDK_JAVA_OPTIONS=-Xmx512m -Dhttps.proxyHost=canyon-egress -Dhttps.proxyPort=3128 -Dhttp.proxyHost=canyon-egress -Dhttp.proxyPort=3128 -Dhttp.nonProxyHosts=app-postgres|localhost|canyon-server|127.0.0.1",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("the app's environment lacks %s:\n%s", want, strings.Join(env, "\n"))
		}
	}
	e, dir = fakes(t)
	_, err = e.Run(context.Background(), runReq)
	must(t, err)
	for _, kv := range strings.Split(read(t, dir, "run-env"), "\n") {
		if name, _, _ := strings.Cut(kv, "="); slices.Contains(proxyVars, name) || name == "JDK_JAVA_OPTIONS" {
			t.Errorf("a /run without egress set %s", kv)
		}
	}
	for name, bad := range map[string]func(*RunRequest){
		"env names HTTPS_PROXY":   func(r *RunRequest) { r.Env = map[string]string{"HTTPS_PROXY": "http://elsewhere:3128"} },
		"env names no_proxy":      func(r *RunRequest) { r.Env = map[string]string{"no_proxy": "*"} },
		"a proxy on another port": func(r *RunRequest) { r.Egress = &Egress{Proxy: "http://canyon-egress:8080", NoProxy: []string{}} },
		"a host named twice": func(r *RunRequest) {
			r.Egress = &Egress{Proxy: "http://canyon-egress:3128", NoProxy: []string{"a", "a"}}
		},
		"a no_proxy wildcard":  func(r *RunRequest) { r.Egress = &Egress{Proxy: "http://canyon-egress:3128", NoProxy: []string{"*"}} },
		"a run that is no run": func(r *RunRequest) { r.Run = "--label=x" },
	} {
		e, dir := fakes(t)
		req := egressReq
		bad(&req)
		if _, err := e.Run(context.Background(), req); !errors.Is(err, ErrRequest) || readIf(dir, "argv") != "" {
			t.Errorf("%s: %v, want a refusal before docker is asked anything", name, err)
		}
	}
}

// contracts/executor.md, Egress: with egress, up also means Canyon's egress gateway answers from the app's
// own namespace; nothing there is the executor's 502, the reason alone, and the container that ran is
// handed back — never an app left running whose calls out have no door.
func TestRunWhoseDoorAnswersNothingKeepsThePrevious(t *testing.T) {
	e, dir := fakes(t)
	dockerNets(t, dir, netJSON(true, map[string]string{"claims-portal-preview": "172.28.3.10"}), false)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	_, err := (&Client{Base: srv.URL}).Run(context.Background(), egressReq)
	argv := read(t, dir, "argv")
	door := "nc\n-z\n-w\n2\ncanyon-egress\n3128\n"
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "nothing answers at canyon-egress:3128, Canyon's egress gateway, from the app's network") {
		t.Fatalf("a /run whose door answers nothing: %v, want 502 naming it", err)
	}
	if i := strings.Index(argv, door); i < 0 || !strings.Contains(argv[i:], "rename\nclaims-portal-preview-previous\nclaims-portal-preview\n") {
		t.Fatalf("the door tried, then the previous container named back:\n%s", argv)
	}
}

// contracts/executor.md, POST /callers on Docker: an address is a caller when exactly one container on a
// canyon-app-<app> network holds it, that container is <app>-<target> or its -previous by name, and the
// address is one of that target's fixed addresses on that network, which is internal; run is its canyon.gate_run. Anything else —
// docker's own address, the other target's slot, a slot on an open network, an address held twice, one held by no app container —
// is left out. since is the container's State.StartedAt, when docker gave it the address, never its Created, which is before; one
// that never started, its StartedAt docker's zero, is left out. In the order asked.
func TestCallersNamesOnlyAFixedAddressHeldOnce(t *testing.T) {
	dir := t.TempDir()
	nets := []appNet{
		{Name: "canyon-app-claims-portal", Internal: true, Containers: map[string]endpoint{
			"c1": {Name: "claims-portal-prod", IPv4Address: "172.28.3.11/24"},
			"c3": {Name: "claims-portal-preview-previous", IPv4Address: "172.28.3.12/24"},
			"c4": {Name: "canyon-egress", IPv4Address: "172.28.3.200/24"},
			"c5": {Name: "claims-portal-preview", IPv4Address: "172.28.3.13/24"}, // prod's slot
		}},
		{Name: "canyon-app-case-tracker", Internal: true, Containers: map[string]endpoint{
			"c6": {Name: "case-tracker-prod", IPv4Address: "172.28.7.13/24"},
			"c7": {Name: "claims-portal-prod", IPv4Address: "172.28.7.11/24"},   // another app's name on this network
			"c2": {Name: "case-tracker-preview", IPv4Address: "172.28.3.11/24"}, // a second claim to claims-portal-prod's address
			"c9": {Name: "case-tracker-preview", IPv4Address: "172.28.7.10/24"}, // held once, never started
		}},
	}
	nets = append(nets, appNet{Name: "canyon-app-ledger", Containers: map[string]endpoint{ // open: its fixed address names no one
		"c8": {Name: "ledger-preview", IPv4Address: "172.28.9.10/24"},
	}})
	nets[0].IPAM.Config, nets[1].IPAM.Config, nets[2].IPAM.Config = []ipamConfig{{Subnet: "172.28.3.0/24"}}, []ipamConfig{{Subnet: "172.28.7.0/24"}}, []ipamConfig{{Subnet: "172.28.9.0/24"}}
	inspected, _ := json.Marshal(nets)
	must(t, os.WriteFile(filepath.Join(dir, "nets"), inspected, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "labels"), []byte(`[{"Id":"c3","Created":"2026-10-11T09:13:58Z","State":{"StartedAt":"2026-10-11T11:14:02.418123456+02:00"},"Config":{"Labels":{"canyon.gate_run":"gr_6f1c0b9a7e2a9d3c","canyon.app":"claims-portal"}}},`+
		`{"Id":"c6","Created":"2026-10-11T09:20:40Z","State":{"StartedAt":"2026-10-11T09:20:47Z"},"Config":{"Labels":{"canyon.gate_run":"--not-a-run"}}},`+
		`{"Id":"c9","Created":"2026-10-11T09:20:40Z","State":{"StartedAt":"0001-01-01T00:00:00Z"},"Config":{"Labels":{}}}]`), 0o600))
	script := `#!/bin/sh
d="` + dir + `"
printf '%s\n' "$@" >> "$d/argv"
[ -e "$d/fail" ] && exit 1
case "$1 $2" in
"network ls") printf 'canyon-app-claims-portal\ncanyon-app-case-tracker\ncanyon-app-ledger\nnot-canyon-app-x\n';;
"network inspect") cat "$d/nets";;
"container inspect") cat "$d/labels";;
esac
`
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700)) //nolint:gosec // test fake
	srv := httptest.NewServer(Handler(&Executor{Docker: filepath.Join(dir, "docker")}))
	defer srv.Close()
	post := func(body string) (int, string) {
		resp, err := srv.Client().Post(srv.URL+"/callers", "application/json", strings.NewReader(body))
		must(t, err)
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	code, body := post(`{"addresses":["172.28.7.13","172.28.3.11","172.28.3.12","172.28.3.200","172.28.3.13","172.28.7.11","172.28.9.10","172.28.7.10","10.0.0.1"]}`)
	want := `{"callers":[{"address":"172.28.7.13","app":"case-tracker","target":"prod","kind":"web","run":null,"since":"2026-10-11T09:20:47Z"},{"address":"172.28.3.12","app":"claims-portal","target":"preview","kind":"web","run":"gr_6f1c0b9a7e2a9d3c","since":"2026-10-11T09:14:02.418123456Z"}]}`
	if code != http.StatusOK || body != want {
		t.Fatalf("POST /callers: %d %s\nwant %s", code, body, want)
	}
	if argv := read(t, dir, "argv"); !strings.Contains(argv, "network\ninspect\ncanyon-app-claims-portal\ncanyon-app-case-tracker\ncanyon-app-ledger\n") || strings.Contains(argv, "not-canyon-app-x") {
		t.Fatalf("one inspect of the app networks alone:\n%s", argv)
	}
	many := make([]string, MaxCallers+1)
	for i := range many {
		many[i] = fmt.Sprintf("%q", fmt.Sprintf("10.%d.%d.1", i/256, i%256))
	}
	for name, body := range map[string]string{
		"none":       `{"addresses":[]}`,
		"twice":      `{"addresses":["172.28.3.11","172.28.3.11"]}`,
		"IPv6":       `{"addresses":["::1"]}`,
		"a name":     `{"addresses":["canyon-server"]}`,
		"leading 0s": `{"addresses":["172.028.3.11"]}`,
		"past 1,024": `{"addresses":[` + strings.Join(many, ",") + `]}`,
		"no JSON":    `addresses`,
	} {
		if code, _ := post(body); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	must(t, os.WriteFile(filepath.Join(dir, "fail"), nil, 0o600))
	if code, body := post(`{"addresses":["172.28.3.11"]}`); code != http.StatusBadGateway {
		t.Fatalf("a daemon that does not answer: %d %s, want 502", code, body)
	}
}

// The contract fixture's run_request, egress and all, is a /run this executor starts: the client and the
// executor agree on the shape (contracts/executor.md, Egress).
func TestHTTPRunRoutesTheFixturesEgress(t *testing.T) {
	raw, err := os.ReadFile("../contracts/testdata/executor/claims-portal.json")
	must(t, err)
	var fixture struct {
		RunRequest json.RawMessage `json:"run_request"`
	}
	must(t, json.Unmarshal(raw, &fixture))
	e, dir := fakes(t)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	resp, err := srv.Client().Post(srv.URL+"/run", "application/json", bytes.NewReader(fixture.RunRequest))
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(read(t, dir, "run-env"), "HTTPS_PROXY=http://canyon-egress:3128\n") {
		t.Fatalf("the fixture's /run: %d %s, want 200 with the app behind Envoy", resp.StatusCode, body)
	}
}

// dockerRecreate replaces fakes' docker with one that keeps canyon-app-claims-portal as <dir>/net, the JSON
// docker network inspect prints — net when not "", none otherwise — and lists the app's containers from
// <dir>/containers, a "name state image" line each. network create writes the network it was asked for,
// refused while <dir>/refuse exists. Every call but a read (container ls, network inspect, network ls) goes
// to <dir>/argv, so argv is what changed, in order.
func dockerRecreate(t *testing.T, dir, net string) {
	t.Helper()
	if net != "" {
		must(t, os.WriteFile(filepath.Join(dir, "net"), []byte(net), 0o600))
	}
	must(t, os.WriteFile(filepath.Join(dir, "containers"), []byte("claims-portal-preview running sha256:aa\nclaims-portal-prod-previous exited sha256:bb\n"), 0o600))
	script := `#!/bin/sh
d="` + dir + `"
case "$1 $2" in
"container ls") case "$*" in *label=canyon.holder*) [ -e "$d/holders" ] && cat "$d/holders"; exit 0;; esac
	for a; do case "$a" in name=*) n=${a#name=^}; grep "^${n%?} " "$d/containers";; esac; done; exit 0;;
"network inspect") [ "$3" = --format ] && [ -e "$d/net" ] && exec cat "$d/net"; exit 1;;
"network ls") exit 0;;
esac
printf '%s\n' "$@" >> "$d/argv"
case "$1 $2" in
"network rm") rm "$d/net";;
"network create") [ -e "$d/refuse" ] && { echo "Error response from daemon: refused"; exit 1; }
	internal=false; sub=; while [ $# -gt 0 ]; do case "$1" in --internal) internal=true;; --subnet) sub=$2;; esac; shift; done
	printf '{"Name":"canyon-app-claims-portal","Internal":%s,"IPAM":{"Config":[{"Subnet":"%s"}]},"Containers":{}}' "$internal" "$sub" > "$d/net";;
esac
exit 0
`
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700)) //nolint:gosec // test fake
}

const (
	stopped    = "stop\nclaims-portal-preview\nclaims-portal-prod-previous\n"
	removed    = "network\nrm\ncanyon-app-claims-portal\n"
	createOpen = "network\ncreate\n--ipv6=false\n--subnet\n172.28.3.0/24\n--gateway\n172.28.3.1\n--label\ncanyon.app=claims-portal\ncanyon-app-claims-portal\n"
	createIn   = "network\ncreate\n--ipv6=false\n--subnet\n172.28.3.0/24\n--gateway\n172.28.3.1\n--internal\n-o\ncom.docker.network.bridge.gateway_mode_ipv4=isolated\n--ip-range\n172.28.3.128/25\n--label\ncanyon.app=claims-portal\ncanyon-app-claims-portal\n"
)

// asked is what the fake was asked to change, "" before anything.
func asked(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	return string(b)
}

func disconnected(c string) string {
	return "network\ndisconnect\ncanyon-app-claims-portal\n" + c + "\n"
}
func connected(c string) string {
	return "network\nconnect\n--gw-priority=-1\ncanyon-app-claims-portal\n" + c + "\n"
}

// contracts/executor.md, POST /networks/recreate (the owner's decision, #2254): either way, the app's containers
// the daemon lists are stopped and left stopped, every container still on the network disconnected, and the
// network removed and created again on its own /24 — internal, its gateway isolated, the door joined, when
// internal is true; open, the door left off, when false — with the joined containers connected again. One
// the app no longer has is made on the pool's first free /24. Nothing is started: the server /runs each env
// again under the network's new spec. The floor holders the stopped containers ran in are removed after the
// stop, before the network is read: none is started again, and the next /run brings its own.
func TestRecreateNetworkMovesAnAppAcrossTheFloorAndStartsNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		internal bool
		net      string
		holders  string
		want     string
	}{
		{"open to internal", true, netJSON(false, map[string]string{"canyon-server": "172.28.3.2"}), "canyon-appfloor-aaaa1111\ncanyon-appfloor-bbbb2222\n",
			stopped + "rm\n-f\ncanyon-appfloor-aaaa1111\ncanyon-appfloor-bbbb2222\n" + disconnected("canyon-server") + removed + createIn + connected("canyon-server") + connected("app-postgres") + connected("canyon-egress")},
		{"internal to open", false, netJSON(true, map[string]string{"canyon-egress": "172.28.3.130"}), "",
			stopped + disconnected("canyon-egress") + removed + createOpen + connected("canyon-server") + connected("app-postgres")},
		{"no network left", true, "", "",
			stopped + strings.ReplaceAll(createIn, "172.28.3.", "172.28.0.") + connected("canyon-server") + connected("app-postgres") + connected("canyon-egress")},
	} {
		e, dir := fakes(t)
		dockerRecreate(t, dir, tc.net)
		if tc.holders != "" {
			must(t, os.WriteFile(filepath.Join(dir, "holders"), []byte(tc.holders), 0o600))
		}
		srv := httptest.NewServer(Handler(e))
		err := (&Client{Base: srv.URL}).RecreateNetwork(context.Background(), "claims-portal", tc.internal)
		srv.Close()
		if argv := asked(dir); err != nil || argv != tc.want {
			t.Errorf("%s: %v; docker was asked, in order:\n%s\nwant:\n%s", tc.name, err, argv, tc.want)
		}
	}
}

// Fail closed (the owner's decision, #2254): a recreation that fails after the stop is a 502 that leaves the
// app's containers stopped — never started again on its old network — and the same call again repeats the
// whole of it, the network the failed one removed made again.
func TestRecreateNetworkThatFailsLeavesTheAppStopped(t *testing.T) {
	e, dir := fakes(t)
	dockerRecreate(t, dir, netJSON(false, map[string]string{"canyon-server": "172.28.3.2"}))
	must(t, os.WriteFile(filepath.Join(dir, "refuse"), nil, 0o600))
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	err := c.RecreateNetwork(context.Background(), "claims-portal", true)
	if argv := asked(dir); err == nil || !strings.Contains(err.Error(), "502") || argv != stopped+disconnected("canyon-server")+removed+createIn {
		t.Fatalf("a refused create: %v, after\n%s\nwant a 502, the app stopped and nothing started or joined", err, argv)
	}
	must(t, os.Remove(filepath.Join(dir, "refuse")))
	must(t, os.Remove(filepath.Join(dir, "argv")))
	if err := c.RecreateNetwork(context.Background(), "claims-portal", true); err != nil {
		t.Fatalf("the same call again: %v", err)
	}
	if argv, want := asked(dir), stopped+strings.ReplaceAll(createIn, "172.28.3.", "172.28.0.")+connected("canyon-server")+connected("app-postgres")+connected("canyon-egress"); argv != want {
		t.Fatalf("the same call again:\n%s\nwant:\n%s", argv, want)
	}
}

// An app the naming pattern refuses is 400 before docker is asked anything; the Kubernetes executor refuses
// the call with 400, its NetworkPolicy written at every /run.
func TestRecreateNetworkRefusals(t *testing.T) {
	e, dir := fakes(t)
	dockerRecreate(t, dir, "")
	for name, h := range map[string]http.Handler{"a bad app": Handler(e), "Kubernetes": Handler(runningFails{})} {
		srv := httptest.NewServer(h)
		app := map[string]string{"a bad app": "../x", "Kubernetes": "claims-portal"}[name]
		err := (&Client{Base: srv.URL}).RecreateNetwork(context.Background(), app, true)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "400") {
			t.Errorf("%s: %v, want 400", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "argv")); !os.IsNotExist(err) {
		t.Errorf("docker was asked something: %v", err)
	}
}
