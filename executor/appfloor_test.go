package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// appFloorCalls runs appfloor.sh as an app's holder does, its iptables-nft, ip6tables-nft and sleep stand-ins
// writing each call to a log, the one named by fail exiting 1: the calls in order, and the script's error.
func appFloorCalls(t *testing.T, fail string) ([]string, error) {
	t.Helper()
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	for _, name := range []string{"iptables-nft", "ip6tables-nft", "sleep"} {
		stub := "#!/bin/sh\necho \"" + name + " $*\" >>" + calls + "\n"
		if fail != "" {
			stub += "case \"" + name + " $*\" in \"" + fail + "\") exit 1;; esac\n"
		}
		must(t, os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o755)) //nolint:gosec // the test's stand-ins, executable
	}
	cmd := exec.Command("sh", "-c", appFloor)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "CANYON_APP_GATEWAY=10.213.4.1", "CANYON_APP_SUBNET=10.213.4.0/24",
		"CANYON_APP_HOST=10.0.0.4/32,203.0.113.7/32", "CANYON_APP_POOLS=10.213.0.0/16,10.214.0.0/16", "CANYON_APPS_ALLOW=10.40.0.0/16:5432,192.168.7.10/32:8081"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("appfloor.sh: %s", out)
	}
	b, _ := os.ReadFile(calls) //nolint:gosec // the test's own temporary file
	return strings.Split(strings.TrimSpace(string(b)), "\n"), err
}

// An app's floor (contracts/executor.md, An app reaches no private, link-local or host address) rejects the
// host — its network's gateway, every address of the Docker host's own, a public one among them, link-local, the
// metadata addresses, loopback and the unroutable ranges — before its own network returns, so neither the /24 nor any CANYON_APPS_ALLOW entry opens them; then
// Canyon's other networks, before any entry; then every private range after the last entry. What is left,
// public addresses, returns. Nothing is forwarded, IPv6 is dropped but loopback, and the jump into its chain
// is its last rule, then sleep.
func TestTheAppFloorRejectsTheHostBeforeItsOwnNetwork(t *testing.T) {
	calls, err := appFloorCalls(t, "")
	if err != nil {
		t.Fatal(err)
	}
	reject := func(r string) string { return "iptables-nft -A CANYON-APP -d " + r + " -j REJECT" }
	own := slices.Index(calls, "iptables-nft -A CANYON-APP -d 10.213.4.0/24 -j RETURN")
	allows := []string{"iptables-nft -A CANYON-APP -p tcp -d 10.40.0.0/16 --dport 5432 -j RETURN", "iptables-nft -A CANYON-APP -p tcp -d 192.168.7.10/32 --dport 8081 -j RETURN"}
	first, last := slices.Index(calls, allows[0]), slices.Index(calls, allows[1])
	if own < 0 || first < own || last < first {
		t.Fatalf("its own network, then the allowed entries, in order, are not rules:\n%s", strings.Join(calls, "\n"))
	}
	for _, r := range []string{"10.213.4.1/32", "10.0.0.4/32", "203.0.113.7/32", "169.254.0.0/16", "168.63.129.16/32", "100.100.100.200/32", "0.0.0.0/8", "127.0.0.0/8", "224.0.0.0/4", "240.0.0.0/4"} {
		if at := slices.Index(calls, reject(r)); at < 0 || at > own {
			t.Errorf("%s is not rejected before the app's own network returns (call %d, its network %d)", r, at, own)
		}
	}
	for _, r := range []string{"10.213.0.0/16", "10.214.0.0/16"} {
		if at := slices.Index(calls, reject(r)); at < own || at > first {
			t.Errorf("%s, Canyon's, is not rejected between the app's own network and the first allowed entry (call %d)", r, at)
		}
	}
	for _, r := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15"} {
		if at := slices.Index(calls, reject(r)); at < last {
			t.Errorf("%s is not rejected after the last allowed entry (call %d, the entry %d)", r, at, last)
		}
	}
	if slices.Contains(calls, "iptables-nft -A CANYON-APP -j REJECT") || slices.ContainsFunc(calls, func(c string) bool { return strings.Contains(c, "--dport 443 -j RETURN") }) || !slices.Contains(calls, "iptables-nft -P FORWARD DROP") {
		t.Errorf("want every public address left to return, as before the floor, and FORWARD dropped:\n%s", strings.Join(calls, "\n"))
	}
	if n := len(calls); n < 2 || calls[n-2] != "iptables-nft -I OUTPUT 1 -j CANYON-APP" || calls[n-1] != "sleep infinity" {
		t.Errorf("want the jump into CANYON-APP the last rule, then sleep:\n%s", strings.Join(calls, "\n"))
	}
	head := []string{"iptables-nft -N CANYON-APP", "iptables-nft -A CANYON-APP -o lo -j RETURN"}
	if len(calls) < 2 || !slices.Equal(calls[:2], head) {
		t.Errorf("want the chain's first rule the namespace's own loopback, where Docker's resolver is:\n%s", strings.Join(calls, "\n"))
	}
	// what it answers returns only after the host is rejected, so no connection to the host outlives the floor
	if est, gw := slices.Index(calls, "iptables-nft -A CANYON-APP -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN"), slices.Index(calls, reject("240.0.0.0/4")); est < gw || est > own {
		t.Errorf("established connections return at call %d: want after the host's rejects (the last %d) and before the app's own network (%d)\n%s", est, gw, own, strings.Join(calls, "\n"))
	}
	if _, err := os.Stat("/proc/net/if_inet6"); err == nil {
		v6 := []string{"ip6tables-nft -N CANYON-APP", "ip6tables-nft -A CANYON-APP -o lo -j RETURN", "ip6tables-nft -A CANYON-APP -j DROP", "ip6tables-nft -I OUTPUT 1 -j CANYON-APP"}
		at := slices.Index(calls, v6[0])
		if at < 0 || at+4 > len(calls) || !slices.Equal(calls[at:at+4], v6) || at > slices.Index(calls, "iptables-nft -I OUTPUT 1 -j CANYON-APP") {
			t.Errorf("want IPv6 dropped but loopback before the jump:\n%s", strings.Join(calls, "\n"))
		}
	} else {
		t.Log("no /proc/net/if_inet6 here: the floor's IPv6 half is asserted on Linux")
	}
}

// A rule the kernel refuses stops the floor where it is: no jump into the chain, so the executor's check never
// finds it, no app ever joins the namespace, and the holder never sleeps.
func TestTheAppFloorStopsAtARefusedRule(t *testing.T) {
	calls, err := appFloorCalls(t, "iptables-nft -A CANYON-APP -d 10.213.4.1/32 -j REJECT")
	if err == nil || slices.Contains(calls, "iptables-nft -I OUTPUT 1 -j CANYON-APP") || slices.Contains(calls, "sleep infinity") {
		t.Fatalf("a refused rule: %v, calls:\n%s", err, strings.Join(calls, "\n"))
	}
}

// An app on an open network runs in its holder's namespace (contracts/executor.md, An app reaches no private,
// link-local or host address): the holder, canyon-appfloor-<nonce>, on the app's network under the app's name,
// never restarted by docker, every capability dropped but NET_ADMIN, labelled the app's and a holder, its
// floor as its entrypoint, told the network's /24 and gateway, Canyon's networks and CANYON_APPS_ALLOW; its
// floor waited for; then the app started in its namespace, naming it. A redeploy's /run then removes the holder
// no container names any more, and never one a container does.
func TestAnAppOnAnOpenNetworkRunsInItsFloorHolder(t *testing.T) {
	e, dir := fakes(t)
	e.AppsAllow = []string{"10.40.0.0/16:5432"}
	e.Holder = "mirror.example/build-image/distroless-iptables@sha256:" + strings.Repeat("7", 64)
	withDocker(t, e, `case "$*" in *label=canyon.holder*) printf 'canyon-appfloor-0dd0dd00\ncanyon-appfloor-5e7a51de\n'; exit 0;; *'.Config.Labels "canyon.floor"'*) echo canyon-appfloor-5e7a51de; exit 0;; esac`)
	_, err := e.Run(t.Context(), runReq)
	must(t, err)
	argv := read(t, dir, "argv")
	holder := regexp.MustCompile(`\nrun\n-d\n--name\n(canyon-appfloor-[0-9a-f]{8})\n`).FindStringSubmatch(argv)
	if holder == nil {
		t.Fatalf("no holder started:\n%s", argv)
	}
	name := holder[1]
	run := "run\n-d\n--name\n" + name + "\n--network\ncanyon-app-claims-portal\n--network-alias\nclaims-portal-preview\n--restart\nno\n--label\ncanyon.app=claims-portal\n--label\ncanyon.target=preview\n--label\ncanyon.holder\n" +
		"--cap-drop\nALL\n--cap-add\nNET_ADMIN\n--security-opt\nno-new-privileges\n--read-only\n--memory\n32m\n--pids-limit\n32\n--entrypoint\n/bin/sh\n" +
		"--env\nCANYON_APP_GATEWAY=172.28.0.1\n--env\nCANYON_APP_SUBNET=172.28.0.0/24\n--env\nCANYON_APP_HOST=127.0.0.1/32,10.0.0.4/32,203.0.113.7/32\n" +
		"--env\nCANYON_APP_POOLS=172.28.0.0/16,10.213.0.0/16,10.214.0.0/16\n--env\nCANYON_APPS_ALLOW=10.40.0.0/16:5432\n" + e.Holder + "\n-c\n" + appFloor
	addrs := "run\n--rm\n--network\nhost\n--user\n65534:65534\n--cap-drop\nALL\n--security-opt\nno-new-privileges\n" + busyboxImage + "\nip\naddr\nshow\n"
	check := "exec\n" + name + "\n/bin/sh\n-c\niptables-nft -C OUTPUT -j CANYON-APP && { [ ! -e /proc/net/if_inet6 ] || ip6tables-nft -C OUTPUT -j CANYON-APP; }\n"
	app := "run\n-d\n--name\nclaims-portal-preview\n--network\ncontainer:" + name + "\n--label\ncanyon.app=claims-portal\n--label\ncanyon.target=preview\n--label\ncanyon.floor=" + name + "\n"
	at := func(s string) int { return strings.Index(argv, s) }
	if locked := at("network\ninspect\n--format\n{{json .}}\ncanyon-app-claims-portal\n"); at(addrs) < 0 || locked < at(addrs) {
		t.Fatalf("want the host's addresses read before the network lock's first call, the app network's inspect:\n%s", argv)
	}
	if at(addrs) < 0 || at(run) < at(addrs) || at(check) < at(run) || at(app) < at(check) {
		t.Fatalf("want the host's addresses read from its own namespace, the holder started told them, its floor checked, then the app in its namespace:\n%s", argv)
	}
	if !strings.Contains(argv[at(app):], "rm\n-f\ncanyon-appfloor-0dd0dd00\n") || strings.Contains(argv, "rm\n-f\ncanyon-appfloor-5e7a51de\n") {
		t.Fatalf("after the start, the holder no container names is removed, and never the one a container names:\n%s", argv)
	}
}

// Short of its floor, an app on an open network is never started: the /run fails, its reason starting
// "app network: ", before anything of the app is set aside or started, and leaves no holder — a network
// with no gateway in its /24, a holder docker does not start, a floor not in place within the bound.
func TestAnAppOnAnOpenNetworkFailsClosedWithoutItsFloor(t *testing.T) {
	floorWait = 500 * time.Millisecond
	t.Cleanup(func() { floorWait = 30 * time.Second })
	for name, c := range map[string]struct{ docker, says string }{
		"no gateway":       {`[ "$1 $2" = "network inspect" ] && { echo '{"Name":"canyon-app-claims-portal","Internal":false,"IPAM":{"Config":[{"Subnet":"172.28.3.0/24","Gateway":"172.28.9.1"}]},"Containers":{"a":{"Name":"canyon-server"},"b":{"Name":"app-postgres"}}}'; exit 0; }`, "is not a /24 with its gateway in it"},
		"holder refused":   {`case "$1 $2 $3 $4" in "run -d --name canyon-appfloor-"*) echo 'Unable to find image' >&2; exit 125;; esac`, "the holder canyon-appfloor-"},
		"floor never set":  {`[ "$1" = exec ] && exit 1; [ "$1" = logs ] && { echo 'iptables-nft: Could not fetch rule set generation id' >&2; exit 0; }`, "was not in place within 500ms: iptables-nft: Could not fetch rule set generation id"},
		"host unread":      {`case "$*" in *" ip addr show") echo 'Unable to find image' >&2; exit 125;; esac`, "the addresses of the Docker host"},
		"host addressless": {`case "$*" in *" ip addr show") echo '1: lo: <LOOPBACK>'; exit 0;; esac`, "the addresses of the Docker host"},
	} {
		t.Run(name, func(t *testing.T) {
			e, dir := fakes(t)
			withDocker(t, e, c.docker)
			_, err := e.Run(t.Context(), runReq)
			if err == nil || !strings.HasPrefix(err.Error(), "app network: ") || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("%v, want app network: … %s", err, c.says)
			}
			argv := readIf(dir, "argv")
			if strings.Contains(argv, "--name\nclaims-portal-preview\n") || strings.Contains(argv, "rename\n") {
				t.Fatalf("the app was set aside or started without its floor:\n%s", argv)
			}
			if started := regexp.MustCompile(`\nrun\n-d\n--name\n(canyon-appfloor-[0-9a-f]{8})\n`).FindStringSubmatch(argv); started != nil && !strings.Contains(argv, "rm\n-f\n"+started[1]+"\n") {
				t.Fatalf("the holder %s was left behind:\n%s", started[1], argv)
			}
		})
	}
}

// An app routed through Canyon's egress gateway runs on an internal network, which has no address of the
// host's and nothing routed out of it: it is started on the network itself, at its fixed address, with no holder.
func TestAnInternalNetworkHasNoHolder(t *testing.T) {
	e, dir := fakes(t)
	dockerNets(t, dir, netJSON(true, map[string]string{"canyon-server": "172.28.3.130"}), true)
	_, err := e.Run(t.Context(), egressReq)
	must(t, err)
	if argv := read(t, dir, "argv"); strings.Contains(argv, "canyon-appfloor-") || !strings.Contains(argv, "run\n-d\n--name\nclaims-portal-preview\n--network\ncanyon-app-claims-portal\n") {
		t.Fatalf("a /run on an internal network started a holder, or not the app on the network:\n%s", argv)
	}
}

// /stop removes the holders no remaining container names, the stopped env's among them, before the network.
func TestStopRemovesTheStoppedAppsHolder(t *testing.T) {
	e, dir := fakes(t)
	withDocker(t, e, `case "$*" in *label=canyon.holder*) printf 'canyon-appfloor-0dd0dd00\ncanyon-appfloor-5e7a51de\n'; exit 0;; *'.Config.Labels "canyon.floor"'*) echo canyon-appfloor-5e7a51de; exit 0;; esac`)
	must(t, e.Stop(t.Context(), "claims-portal", "preview"))
	if argv := read(t, dir, "argv"); !strings.Contains(argv, "rm\n-f\ncanyon-appfloor-0dd0dd00\n") || strings.Contains(argv, "rm\n-f\ncanyon-appfloor-5e7a51de\n") {
		t.Fatalf("the stopped app's holder stays, or a running one's goes:\n%s", argv)
	}
}

// reconcileFake is a docker whose containers are <dir>/s/<name>, each the JSON docker container inspect prints of
// one, and whose networks are <dir>/n/<name>, each network inspect's JSON. Its listing of label canyon.app is
// <dir>/apps; of label canyon.holder, every container whose JSON carries that label; by name, those that exist.
// An inspect of several prints those that exist and exits 1 if any does not, as docker does. exec, the floor
// check, fails while <dir>/nofloor exists; start, restart, stop and rm go to <dir>/acts, in order. stop and start
// keep the container's state: a start fails while <dir>/failstart-<name> exists, a stop while <dir>/failstop-<name>
// does, leaving it running, and, as docker's, an app's start while its holder is not running is refused, the refusal
// recorded as its State.Error. Once a stop of <name> was asked, its inspect fails while <dir>/failinspect-<name> exists.
func reconcileFake(t *testing.T, containers map[string]string, apps ...string) (*Executor, string) {
	t.Helper()
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "s"), 0o700))
	must(t, os.MkdirAll(filepath.Join(dir, "n"), 0o700))
	for name, j := range containers {
		must(t, os.WriteFile(filepath.Join(dir, "s", name), []byte(j), 0o600))
	}
	must(t, os.WriteFile(filepath.Join(dir, "apps"), []byte(strings.Join(apps, "\n")+"\n"), 0o600))
	script := `#!/bin/sh
d="` + dir + `"
edit() { sed "$1" "$2" > "$d/edit.tmp" && mv "$d/edit.tmp" "$2"; }
printf '%s\n' "$@" >> "$d/argv"
case "$1 $2" in
"container ls") case "$*" in
	*label=canyon.holder*) for f in "$d"/s/*; do grep -q '"canyon.holder"' "$f" && basename "$f"; done; exit 0;;
	*name=*) for a; do case "$a" in name=*) n=${a#name=^}; n=${n%?}; [ -e "$d/s/$n" ] && echo "$n running sha256:ab";; esac; done; exit 0;;
	esac; cat "$d/apps"; exit 0;;
"container inspect") shift 2; rc=0; [ "$1" = -- ] && shift; if [ "$1" = --format ]; then shift 2; for n; do sed -n 's/.*"canyon.floor":"\([^"]*\)".*/\1/p' "$d/s/$n"; done; exit 0; fi
	sep=; printf '['; for n; do if [ -e "$d/failinspect-$n" ] && [ -e "$d/stopped-$n" ]; then echo "Error: the daemon did not answer" >&2; rc=1
	elif [ -e "$d/s/$n" ]; then printf '%s%s' "$sep" "$(cat "$d/s/$n")"; sep=,; else echo "Error: No such container: $n" >&2; rc=1; fi; done; printf ']'; exit $rc;;
"network inspect") shift 2; sep=; printf '['; for n; do [ -e "$d/n/$n" ] && { printf '%s%s' "$sep" "$(cat "$d/n/$n")"; sep=,; }; done; printf ']'; exit 0;;
"network ls") exit 0;;
esac
case "$1" in
exec) [ -e "$d/nofloor" ] && exit 1; exit 0;;
logs) echo 'iptables-nft: Could not fetch rule set generation id'; exit 0;;
rm) echo "$*" >> "$d/acts"; for n; do rm -f "$d/s/$n"; done; exit 0;;
restart|stop|start) v=$1; n=$2; [ "$n" = -- ] && n=$3;;
esac
case "$v" in
restart) echo "$v $n" >> "$d/acts"; exit 0;;
stop) echo "$v $n" >> "$d/acts"; : > "$d/stopped-$n"; [ -e "$d/failstop-$n" ] && exit 1; edit 's/"Running":true/"Running":false/' "$d/s/$n"; exit 0;;
start) echo "$v $n" >> "$d/acts"; f="$d/s/$n"
	h=$(sed -n 's/.*"canyon.floor":"\([^"]*\)".*/\1/p' "$f") # as docker does: an app whose holder is not running is refused, the refusal its error
	if [ -n "$h" ] && [ -e "$d/s/$h" ] && grep -q '"Running":false' "$d/s/$h"; then
		edit 's/"Error":"[^"]*"/"Error":"cannot join network namespace of a non running container"/' "$f"; exit 1
	fi
	[ -e "$d/failstart-$n" ] && exit 1 # a start that fails before the daemon records anything: a client cut off
	[ -e "$d/slowstart-$n" ] && { : > "$d/starting-$n"; sleep 0.5; }
	edit 's/"Running":false/"Running":true/' "$f"; exit 0;;
esac
exit 1
`
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700)) //nolint:gosec // test fake
	return &Executor{Docker: filepath.Join(dir, "docker")}, dir
}

// inspectJSON is docker container inspect's JSON of one container: its name and ID, running or not, when it last
// started, the error of its last start, its labels and its network mode.
func inspectJSON(t *testing.T, name, id string, running bool, started, err string, labels map[string]string, mode string) string {
	t.Helper()
	var c floorState
	c.Name, c.ID, c.HostConfig.NetworkMode, c.Config.Labels = "/"+name, id, mode, labels
	c.State.Running, c.State.Error = running, err
	at, perr := time.Parse(time.RFC3339Nano, started)
	must(t, perr)
	c.State.StartedAt = at
	b, merr := json.Marshal(c)
	must(t, merr)
	return string(b)
}

const (
	floorHolder   = "canyon-appfloor-aaaa1111"
	floorHolderID = "f00d" + "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa"
)

// appState is claims-portal-prod's inspect, run as /run runs it: its holder named in canyon.floor and joined.
func appState(t *testing.T, running bool, started, err string) string {
	return inspectJSON(t, "claims-portal-prod", "a99"+strings.Repeat("0", 61), running, started, err,
		map[string]string{"canyon.app": "claims-portal", "canyon.target": "prod", "canyon.floor": floorHolder}, "container:"+floorHolderID)
}

// holderState is claims-portal-prod's holder's inspect, as holdFloor starts it.
func holderState(t *testing.T, running bool, started string) string {
	return inspectJSON(t, floorHolder, floorHolderID, running, started, "",
		map[string]string{"canyon.app": "claims-portal", "canyon.target": "prod", "canyon.holder": ""}, "canyon-app-claims-portal")
}

// The executor brings an app that runs in a holder back, floored, in order (contracts/executor.md, An app
// reaches no private, link-local or host address): docker restarts neither a holder nor, with its holder down, the
// app. A holder down under a running app — its namespace emptied — or under an app whose start docker refused
// for want of it — a daemon's restart, the host's reboot — is started, its floor waited for, then the app started
// again; a holder started after the app is waited for, then the app restarted. An app that stopped by itself, a
// holder that is gone, a -previous and an app in order are left as they are. A floor not in place in time stops
// the holder again and starts no app.
func TestReconcileBringsAnAppBackFlooredInOrder(t *testing.T) {
	floorWait = 300 * time.Millisecond
	t.Cleanup(func() { floorWait = 30 * time.Second })
	const (
		t1      = "2026-10-26T10:00:00.000000001Z"
		t2      = "2026-10-26T10:00:01.5Z"
		t3      = "2026-10-26T11:00:00Z"
		refused = "cannot join network namespace of a non running container: container canyon-appfloor-aaaa1111 is exited"
		h       = floorHolder
	)
	for _, tc := range []struct {
		name    string
		holder  string // its state, "" for gone
		app     string
		nofloor bool
		want    string
	}{
		{"the holder down under the running app", holderState(t, false, t1), appState(t, true, t2, ""), false, "stop claims-portal-prod\nstart " + h + "\nstart claims-portal-prod\n"},
		{"a reboot: docker's start of the app refused for want of its holder", holderState(t, false, t1), appState(t, false, t2, refused), false, "stop claims-portal-prod\nstart " + h + "\nstart claims-portal-prod\n"},
		{"a reboot, the floor never in place", holderState(t, false, t1), appState(t, false, t2, refused), true, "stop claims-portal-prod\nstart " + h + "\nstop " + h + "\nstart claims-portal-prod\n"},
		{"the holder started again after the app", holderState(t, true, t3), appState(t, true, t2, ""), false, "stop claims-portal-prod\nstart claims-portal-prod\n"},
		{"the holder running, the app's start refused", holderState(t, true, t1), appState(t, false, t2, refused), false, "stop claims-portal-prod\nstart claims-portal-prod\n"},
		{"the holder running, the app's start refused, the floor not in place", holderState(t, true, t1), appState(t, false, t2, refused), true, "stop claims-portal-prod\nstop " + h + "\nstart claims-portal-prod\n"},
		{"an app that stopped by itself, past its restarts, before a reboot", holderState(t, false, t1), appState(t, false, t2, ""), false, ""},
		{"an app that stopped by itself beside its running holder", holderState(t, true, t1), appState(t, false, t2, ""), false, ""},
		{"both running, in order", holderState(t, true, t1), appState(t, true, t2, ""), false, ""},
		{"the holder gone, as a recreated network leaves it", "", appState(t, false, t2, refused), false, ""},
	} {
		containers := map[string]string{"claims-portal-prod": tc.app}
		listed := []string{"claims-portal-prod"}
		if tc.holder != "" {
			containers[h] = tc.holder
			listed = append(listed, h)
		}
		e, dir := reconcileFake(t, containers, listed...)
		if tc.nofloor {
			must(t, os.WriteFile(filepath.Join(dir, "nofloor"), nil, 0o600))
		}
		e.Reconcile(context.Background())
		if got := readIf(dir, "acts"); got != tc.want {
			t.Errorf("%s: docker was told\n%s\nwant\n%s", tc.name, got, tc.want)
		}
	}
	// a container a /run set aside is the /run's: never reconciled, whatever its state
	e, dir := reconcileFake(t, map[string]string{"claims-portal-prod-previous": appState(t, true, t2, ""), h: holderState(t, false, t1)}, "claims-portal-prod-previous", h)
	e.Reconcile(context.Background())
	if got := readIf(dir, "acts"); got != "" {
		t.Errorf("a -previous was reconciled, or its holder reaped: %s", got)
	}
}

// Each pass removes, under the lock, a holder no container of its app names — one a crash between a holder's
// start and its app's left, which would go on answering the app's alias — and never one a container names.
func TestReconcileReapsAHolderNoAppNames(t *testing.T) {
	const t1, t2, orphan = "2026-10-26T10:00:00Z", "2026-10-26T10:00:01Z", "canyon-appfloor-0dd0dd00"
	orphaned := inspectJSON(t, orphan, "0dd0"+strings.Repeat("0", 60), true, t1, "",
		map[string]string{"canyon.app": "claims-portal", "canyon.target": "prod", "canyon.holder": ""}, "canyon-app-claims-portal")
	e, dir := reconcileFake(t, map[string]string{"claims-portal-prod": appState(t, true, t2, ""), floorHolder: holderState(t, true, t1), orphan: orphaned},
		"claims-portal-prod", floorHolder, orphan)
	e.Reconcile(context.Background())
	if got := readIf(dir, "acts"); got != "rm -f "+orphan+"\n" {
		t.Errorf("docker was told %q; want the orphan holder removed alone", got)
	}
}

// Once a reconcile has stopped the app, a later failure — its holder's start, or the floor not in place, whether it
// started the holder or found it running — never leaves the app stopped with no error, which no pass would start
// again: the holder is left stopped and docker asked to start the app, which it refuses, recording the error; and
// the next pass, the fault gone, brings both back.
func TestAReconcileThatStoppedTheAppLeavesItsErrorForTheNextPass(t *testing.T) {
	floorWait = 300 * time.Millisecond
	t.Cleanup(func() { floorWait = 30 * time.Second })
	const t1, t2, t3, h = "2026-10-26T10:00:00Z", "2026-10-26T10:00:01Z", "2026-10-26T11:00:00Z", floorHolder
	for _, tc := range []struct {
		name, fault, first string
		holder             string
	}{
		{"the holder down under the running app, its start failing", "failstart-" + h,
			"stop claims-portal-prod\nstart " + h + "\nstop " + h + "\nstart claims-portal-prod\n", holderState(t, false, t1)},
		{"the holder down under the running app, its floor not in place", "nofloor",
			"stop claims-portal-prod\nstart " + h + "\nstop " + h + "\nstart claims-portal-prod\n", holderState(t, false, t1)},
		{"the holder started after the app, its floor not in place", "nofloor",
			"stop claims-portal-prod\nstop " + h + "\nstart claims-portal-prod\n", holderState(t, true, t3)},
	} {
		e, dir := reconcileFake(t, map[string]string{"claims-portal-prod": appState(t, true, t2, ""), h: tc.holder}, "claims-portal-prod", h)
		must(t, os.WriteFile(filepath.Join(dir, tc.fault), nil, 0o600))
		e.Reconcile(context.Background())
		if got := readIf(dir, "acts"); got != tc.first {
			t.Errorf("%s: the first pass told docker\n%s\nwant\n%s", tc.name, got, tc.first)
		}
		must(t, os.Remove(filepath.Join(dir, tc.fault)))
		must(t, os.Remove(filepath.Join(dir, "acts")))
		e.Reconcile(context.Background())
		if got, want := readIf(dir, "acts"), "stop claims-portal-prod\nstart "+h+"\nstart claims-portal-prod\n"; got != want {
			t.Errorf("%s: the next pass told docker\n%s\nwant both brought back:\n%s", tc.name, got, want)
		}
	}
}

// A pass whose own time runs out after the floor is in place never cuts the app's start short (#2523): each
// action has a deadline of its own, so the app stopped for its holder is started again in the same pass. And a
// final start that fails before docker records anything goes the way of any failure after the app's stop: the
// holder stopped, the app's start refused, its error recorded, so the next pass brings both back. Every holder
// and app the reconcile names to docker follows --.
func TestAReconcileNeverLeavesTheAppStoppedWhenItsStartIsCut(t *testing.T) {
	const t1, t2, h = "2026-10-26T10:00:00Z", "2026-10-26T10:00:01Z", floorHolder
	// the pass's context ends while the app's start is under way
	e, dir := reconcileFake(t, map[string]string{"claims-portal-prod": appState(t, true, t2, ""), h: holderState(t, false, t1)}, "claims-portal-prod", h)
	must(t, os.WriteFile(filepath.Join(dir, "slowstart-claims-portal-prod"), nil, 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { // the pass's context ends once the app's start has begun
		for _, err := os.Stat(filepath.Join(dir, "starting-claims-portal-prod")); err != nil && ctx.Err() == nil; _, err = os.Stat(filepath.Join(dir, "starting-claims-portal-prod")) {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	e.Reconcile(ctx)
	cancel()
	if app, _ := os.ReadFile(filepath.Join(dir, "s", "claims-portal-prod")); !strings.Contains(string(app), `"Running":true`) { //nolint:gosec // test dir
		t.Errorf("the pass's context ended during the app's start, and the app is not running: %s\ndocker was told\n%s", app, readIf(dir, "acts"))
	}
	if argv := readIf(dir, "argv"); !strings.Contains(argv, "start\n--\n"+h+"\n") || !strings.Contains(argv, "start\n--\nclaims-portal-prod\n") {
		t.Errorf("the holder or the app was handed docker's start without --:\n%s", argv)
	}
	// a final start that fails, docker recording nothing
	e, dir = reconcileFake(t, map[string]string{"claims-portal-prod": appState(t, true, t2, ""), h: holderState(t, false, t1)}, "claims-portal-prod", h)
	must(t, os.WriteFile(filepath.Join(dir, "failstart-claims-portal-prod"), nil, 0o600))
	e.Reconcile(context.Background())
	if got, want := readIf(dir, "acts"), "stop claims-portal-prod\nstart "+h+"\nstart claims-portal-prod\nstop "+h+"\nstart claims-portal-prod\n"; got != want {
		t.Errorf("a failed final start: docker was told\n%s\nwant the holder stopped and the app's start refused after it:\n%s", got, want)
	}
	must(t, os.Remove(filepath.Join(dir, "failstart-claims-portal-prod")))
	must(t, os.Remove(filepath.Join(dir, "acts")))
	e.Reconcile(context.Background())
	if got, want := readIf(dir, "acts"), "stop claims-portal-prod\nstart "+h+"\nstart claims-portal-prod\n"; got != want {
		t.Errorf("after a failed final start, the next pass told docker\n%s\nwant both brought back:\n%s", got, want)
	}
}

// Fail closed: the deliberate refused start of the app is made only once its holder is known stopped. A holder
// whose stop fails, and stays running without its floor, or whose state cannot be read after the stop, never has
// the app started into it: the app stays stopped, logged.
func TestAReconcileNeverStartsTheAppBesideAHolderNotKnownStopped(t *testing.T) {
	floorWait = 300 * time.Millisecond
	t.Cleanup(func() { floorWait = 30 * time.Second })
	const t1, t2, t3, h = "2026-10-26T10:00:00Z", "2026-10-26T10:00:01Z", "2026-10-26T11:00:00Z", floorHolder
	for _, fault := range []string{"failstop-" + h, "failinspect-" + h} {
		e, dir := reconcileFake(t, map[string]string{"claims-portal-prod": appState(t, true, t2, ""), h: holderState(t, true, t3)}, "claims-portal-prod", h)
		for _, f := range []string{"nofloor", fault} {
			must(t, os.WriteFile(filepath.Join(dir, f), nil, 0o600))
		}
		e.Reconcile(context.Background())
		if got, want := readIf(dir, "acts"), "stop claims-portal-prod\nstop "+h+"\n"; got != want {
			t.Errorf("%s: docker was told\n%s\nwant the app stopped, the holder's stop tried, and no start of the app:\n%s", fault, got, want)
		}
	}
}

// An app an executor from before the floor started runs on its open network directly: the first pass that finds
// it logs it once a process, naming the network and the fix, and no pass starts or stops anything. One on an internal network, behind Envoy, is not logged.
func TestReconcileLogsAnAppDeployedBeforeTheFloor(t *testing.T) {
	const t2 = "2026-10-26T10:00:01Z"
	for _, internal := range []bool{false, true} {
		old := inspectJSON(t, "claims-portal-prod", "a99"+strings.Repeat("0", 61), true, t2, "",
			map[string]string{"canyon.app": "claims-portal", "canyon.target": "prod"}, "canyon-app-claims-portal")
		e, dir := reconcileFake(t, map[string]string{"claims-portal-prod": old}, "claims-portal-prod")
		n := appNet{Name: "canyon-app-claims-portal", Internal: internal}
		b, err := json.Marshal(n)
		must(t, err)
		must(t, os.WriteFile(filepath.Join(dir, "n", "canyon-app-claims-portal"), b, 0o600))
		var logged bytes.Buffer
		log.SetOutput(&logged)
		for range 3 { // once a process, on the first pass that finds it, never every 15 s
			e.Reconcile(context.Background())
		}
		log.SetOutput(os.Stderr)
		said := strings.Count(logged.String(), "claims-portal-prod runs on canyon-app-claims-portal with no floor, deployed before the floor: redeploy it")
		if want := map[bool]int{false: 1, true: 0}[internal]; said != want || readIf(dir, "acts") != "" {
			t.Errorf("internal %t: logged %d times (%q), docker told %q; want it said %d times, nothing done", internal, said, logged.String(), readIf(dir, "acts"), want)
		}
	}
}

// The executor's loop reconciles: one pass of Rejoin, against a daemon after a restart, starts the holder, then the app.
func TestRejoinReconciles(t *testing.T) {
	const t1, t2 = "2026-10-26T10:00:00Z", "2026-10-26T10:00:01Z"
	e, dir := reconcileFake(t, map[string]string{"claims-portal-prod": appState(t, false, t2, "cannot join network namespace of a non running container"),
		floorHolder: holderState(t, false, t1)}, "claims-portal-prod", floorHolder)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Rejoin(ctx); close(done) }()
	want := "stop claims-portal-prod\nstart " + floorHolder + "\nstart claims-portal-prod\n"
	for deadline := time.Now().Add(10 * time.Second); readIf(dir, "acts") != want && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if got := readIf(dir, "acts"); got != want {
		t.Fatalf("one pass of Rejoin told docker %q, want %q", got, want)
	}
}

// A redeploy on an open network whose start fails never names back a container that runs in no holder — one an
// executor from before the floor started: it is removed, and the app left down, the start's failure answered.
// One that runs in its holder is named back, as before.
func TestARunNeverNamesAnUnflooredContainerBack(t *testing.T) {
	for _, mode := range []string{"canyon-app-claims-portal", "container:" + floorHolderID} {
		e, dir := fakes(t)
		withDocker(t, e, `case "$*" in *' nc -z '*) exit 1;; *"{{.HostConfig.NetworkMode}}"*) echo '`+mode+`'; exit 0;; esac`)
		_, err := e.Run(t.Context(), runReq)
		var exited *Exited
		if !errors.As(err, &exited) {
			t.Fatalf("%s: %v, want the deaf start's own failure", mode, err)
		}
		argv := read(t, dir, "argv")
		tail := argv[strings.LastIndex(argv, "logs\n--tail\n"):]
		back, removed := strings.Contains(tail, "rename\nclaims-portal-preview-previous\nclaims-portal-preview\n"), strings.Contains(tail, "rm\n-f\nclaims-portal-preview-previous\n")
		if floored := strings.HasPrefix(mode, "container:"); back != floored || removed == floored {
			t.Errorf("%s: named back %t, removed %t; want an unfloored one removed, a floored one named back", mode, back, removed)
		}
	}
}

// The reconcile starts and stops a holder only once it has proved it is this app's own: canyon.floor is a label an
// image may carry too, so it is never believed alone. The container it names must be named canyon-appfloor-<8 hex>,
// carry canyon.holder and this app's canyon.app and canyon.target, and be the very container whose namespace docker
// joined the app to (HostConfig.NetworkMode, container:<its ID>), which the image cannot set. Anything else is
// refused and nothing is started or stopped.
func TestReconcileActsOnlyOnTheAppsOwnHolder(t *testing.T) {
	floorWait = 300 * time.Millisecond
	t.Cleanup(func() { floorWait = 30 * time.Second })
	const (
		t1      = "2026-10-26T10:00:00Z"
		t2      = "2026-10-26T10:00:01Z"
		refused = "cannot join network namespace of a non running container"
		victim  = "c0ffee" + "0000000000000000000000000000000000000000000000000000000000"
	)
	app := func(floor, mode string) string {
		return inspectJSON(t, "claims-portal-prod", "a99"+strings.Repeat("0", 61), false, t2, refused,
			map[string]string{"canyon.app": "claims-portal", "canyon.target": "prod", "canyon.floor": floor}, mode)
	}
	holderLabels := func(app, target string) map[string]string {
		return map[string]string{"canyon.app": app, "canyon.target": target, "canyon.holder": ""}
	}
	for _, tc := range []struct {
		name, named string
		containers  map[string]string
	}{
		{"an image's label naming a container of Canyon's", "canyon-server", map[string]string{
			"claims-portal-prod": app("canyon-server", "container:"+victim),
			"canyon-server":      inspectJSON(t, "canyon-server", victim, false, t1, "", map[string]string{}, "bridge")}},
		{"a holder's name, no holder label", floorHolder, map[string]string{
			"claims-portal-prod": app(floorHolder, "container:"+floorHolderID),
			floorHolder:          inspectJSON(t, floorHolder, floorHolderID, false, t1, "", map[string]string{"canyon.app": "claims-portal", "canyon.target": "prod"}, "bridge")}},
		{"another app's holder", floorHolder, map[string]string{
			"claims-portal-prod": app(floorHolder, "container:"+floorHolderID),
			floorHolder:          inspectJSON(t, floorHolder, floorHolderID, false, t1, "", holderLabels("quote-engine", "prod"), "bridge")}},
		{"the other target's holder", floorHolder, map[string]string{
			"claims-portal-prod": app(floorHolder, "container:"+floorHolderID),
			floorHolder:          inspectJSON(t, floorHolder, floorHolderID, false, t1, "", holderLabels("claims-portal", "preview"), "bridge")}},
		{"a holder the app was never joined to", floorHolder, map[string]string{
			"claims-portal-prod": app(floorHolder, "container:"+victim),
			floorHolder:          inspectJSON(t, floorHolder, floorHolderID, false, t1, "", holderLabels("claims-portal", "prod"), "bridge")}},
		{"holder labels on a container not named as a holder", "victim", map[string]string{
			"claims-portal-prod": app("victim", "container:"+victim),
			"victim":             inspectJSON(t, "victim", victim, false, t1, "", holderLabels("claims-portal", "prod"), "bridge")}},
	} {
		e, dir := reconcileFake(t, tc.containers, "claims-portal-prod")
		var logged bytes.Buffer
		log.SetOutput(&logged)
		e.Reconcile(context.Background())
		log.SetOutput(os.Stderr)
		if got := readIf(dir, "acts"); got != "" || !strings.Contains(logged.String(), "claims-portal-prod") || !strings.Contains(logged.String(), tc.named) {
			t.Errorf("%s: docker was told %q; want nothing, and the refusal logged naming the app and %s: %q", tc.name, got, tc.named, logged.String())
		}
	}
	// a label that looks like a flag is handed docker as a name, after --, never read as one
	const flag = "--format={{.ID}}"
	e, dir := reconcileFake(t, map[string]string{"claims-portal-prod": app(flag, "container:"+victim)}, "claims-portal-prod")
	e.Reconcile(context.Background())
	if argv := readIf(dir, "argv"); !strings.Contains(argv, "container\ninspect\n--\n"+flag+"\n") || readIf(dir, "acts") != "" {
		t.Errorf("an image's canyon.floor %q was not handed docker after --, or something was done:\n%s", flag, argv)
	}
}

// Every app container carries canyon.floor as /run sets it — the holder's name, or empty on an internal network —
// so a label its image carries never stands: docker lets a run's own label override the image's.
func TestRunSetsTheFloorLabelOnEveryApp(t *testing.T) {
	e, dir := fakes(t)
	dockerNets(t, dir, netJSON(true, map[string]string{"canyon-server": "172.28.3.130"}), true)
	_, err := e.Run(t.Context(), egressReq)
	must(t, err)
	if argv := read(t, dir, "argv"); !strings.Contains(argv, "--label\ncanyon.floor=\n") {
		t.Fatalf("an app on an internal network runs without an empty canyon.floor of /run's own:\n%s", argv)
	}
}

// A reap removes only a holder: a container that carries canyon.holder and the app's canyon.app but is not named
// canyon-appfloor-<8 hex> — the app's own container, whose image may carry the label — is never removed.
func TestAReapRemovesOnlyAHolder(t *testing.T) {
	e, dir := fakes(t)
	withDocker(t, e, `case "$*" in *label=canyon.holder*) printf 'claims-portal-preview\ncanyon-appfloor-0dd0dd00\ncanyon-appfloor-12345678x\n'; exit 0;; *'.Config.Labels "canyon.floor"'*) exit 0;; esac`)
	must(t, e.Stop(t.Context(), "claims-portal", "prod"))
	argv := read(t, dir, "argv")
	if !strings.Contains(argv, "rm\n-f\ncanyon-appfloor-0dd0dd00\n") || strings.Contains(argv, "rm\n-f\nclaims-portal-preview\n") || strings.Contains(argv, "canyon-appfloor-12345678x") {
		t.Fatalf("a reap removed what is no holder, or kept one that is:\n%s", argv)
	}
}
