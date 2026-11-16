package executor

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// withDocker puts script into e's docker fake ahead of floorFake: it answers first, the fake the rest.
func withDocker(t *testing.T, e *Executor, script string) {
	t.Helper()
	docker, err := os.ReadFile(e.Docker)
	must(t, err)
	must(t, os.WriteFile(e.Docker, []byte(strings.Replace(string(docker), floorFake, script+"\n"+floorFake, 1)), 0o700)) //nolint:gosec // test fake
}

// A build starts its holder on canyon-builds, as contracts/executor.md says it: its floor as its entrypoint, every
// capability dropped but NET_ADMIN, never restarted, labelled canyon.build, canyon-builds' subnet, the apps pool
// and CANYON_BUILD_ALLOW in its environment; it waits for both jumps of the floor; then pack runs every phase in
// the holder's namespace; and the holder is removed once pack is done.
func TestBuildRunsInsideItsFloor(t *testing.T) {
	e, dir := fakes(t)
	e.Allow = []string{"10.20.0.0/16:443", "192.168.7.10/32:8081"}
	e.Holder = "mirror.example/build-image/distroless-iptables@sha256:" + strings.Repeat("7", 64)
	must(t, os.WriteFile(e.Pack, []byte("#!/bin/sh\nprintf '%s\\n' pack \"$@\" >> '"+dir+"/argv'\necho '===> BUILDING'\n"), 0o700)) //nolint:gosec // test fake: pack's argv into docker's, in order
	if _, err := e.Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard); err != nil {
		t.Fatal(err)
	}
	argv := readIf(dir, "argv")
	holder := regexp.MustCompile(`\nrun\n-d\n--name\n(canyon-build-[0-9a-f]{8})\n`).FindStringSubmatch(argv)
	if holder == nil {
		t.Fatalf("no holder started:\n%s", argv)
	}
	name := holder[1]
	run := "run\n-d\n--name\n" + name + "\n--network\ncanyon-builds\n--restart\nno\n--label\ncanyon.build\n--cap-drop\nALL\n--cap-add\nNET_ADMIN\n--security-opt\nno-new-privileges\n--read-only\n--memory\n64m\n--pids-limit\n32\n"
	env := "--entrypoint\n/bin/sh\n--env\nCANYON_BUILD_SUBNET=172.18.0.0/16\n--env\nCANYON_BUILD_HOST=127.0.0.1/32,10.0.0.4/32,203.0.113.7/32\n--env\nCANYON_BUILD_POOLS=172.28.0.0/16,10.213.0.0/16,10.214.0.0/16\n--env\nCANYON_BUILD_ALLOW=10.20.0.0/16:443,192.168.7.10/32:8081\n" + e.Holder + "\n-c\n" + buildFloor
	addrs := "run\n--rm\n--network\nhost\n--user\n65534:65534\n--cap-drop\nALL\n--security-opt\nno-new-privileges\n" + busyboxImage + "\nip\naddr\nshow\n"
	check := "exec\n" + name + "\n/bin/sh\n-c\niptables-nft -C OUTPUT -j CANYON-BUILD && { [ ! -e /proc/net/if_inet6 ] || ip6tables-nft -C OUTPUT -j CANYON-BUILD; }\n"
	pack := "\n--network\ncontainer:" + name + "\n"
	rm := "rm\n-f\n" + name + "\n"
	at := func(s string) int { return strings.Index(argv, s) }
	if at(addrs) < 0 || at(run) < at(addrs) || at(env) != at(run)+len(run) || at(check) < at(env) || at("pack\nbuild\n") < at(check) || at(pack) < at("pack\nbuild\n") || at(rm) < at(pack) {
		t.Fatalf("want the host's addresses read, the holder started told them, its floor checked, pack in its namespace, then the holder removed; docker and pack argv:\n%s", argv)
	}
}

// Short of its floor, a build fails before pack runs, its line starting "build network: ", and leaves no holder:
// canyon-builds cannot be created; a network of that name is not Canyon's, which is refused and left as it is; the
// holder does not start; its floor is not in place within the bound.
func TestBuildFailsClosedWithoutItsFloor(t *testing.T) {
	floorWait = 500 * time.Millisecond
	t.Cleanup(func() { floorWait = 30 * time.Second })
	net := func(driver, ipv6, internal, labels, icc, subnets string) string {
		return `[ "$1 $2 $5" = "network inspect canyon-builds" ] && { echo '{"Name":"canyon-builds","Driver":"` + driver + `","EnableIPv6":` + ipv6 + `,"Internal":` + internal +
			`,"Labels":` + labels + `,"Options":{"com.docker.network.bridge.enable_icc":"` + icc + `"},"IPAM":{"Config":[` + subnets + `]}}'; exit 0; }`
	}
	ours := `{"canyon.build":""}`
	one := `{"Subnet":"172.18.0.0/16"}`
	for name, c := range map[string]struct{ docker, says string }{
		"no network":      {`case "$1 $2" in "network inspect") exit 1;; "network create") echo 'could not find an available, non-overlapping IPv4 address pool' >&2; exit 1;; esac`, "build network: network create canyon-builds: exit status 1: could not find an available"},
		"another driver":  {net("macvlan", "false", "false", ours, "false", one), "is not Canyon's"},
		"IPv6 on":         {net("bridge", "true", "false", ours, "false", one), "is not Canyon's"},
		"internal":        {net("bridge", "false", "true", ours, "false", one), "is not Canyon's"},
		"not labelled":    {net("bridge", "false", "false", `{}`, "false", one), "is not Canyon's"},
		"ICC on":          {net("bridge", "false", "false", ours, "true", one), "is not Canyon's"},
		"two subnets":     {net("bridge", "false", "false", ours, "false", one+`,{"Subnet":"10.9.0.0/16"}`), "is not Canyon's"},
		"no IPv4 subnet":  {net("bridge", "false", "false", ours, "false", `{"Subnet":"fd00::/64"}`), "is not Canyon's"},
		"holder refused":  {`case "$1 $2 $3 $4" in "run -d --name canyon-build-"*) echo 'Unable to find image' >&2; exit 125;; esac`, "build network: the holder canyon-build-"},
		"floor never set": {`[ "$1" = exec ] && exit 1; [ "$1" = logs ] && { echo 'iptables-nft: Could not fetch rule set generation id' >&2; exit 0; }`, "was not in place within 500ms: iptables-nft: Could not fetch rule set generation id"},
		"host unread":     {`case "$*" in *" ip addr show") echo 'Unable to find image' >&2; exit 125;; esac`, "build network: the addresses of the Docker host"},
	} {
		t.Run(name, func(t *testing.T) {
			e, dir := fakes(t)
			withDocker(t, e, c.docker)
			_, err := e.Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard)
			if err == nil || !strings.HasPrefix(err.Error(), "build network: ") || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("%v, want build network: … %s", err, c.says)
			}
			if packed := readIf(dir, "pack-argv"); packed != "" {
				t.Fatalf("pack ran without its floor: %q", packed)
			}
			argv := readIf(dir, "argv")
			if started := regexp.MustCompile(`\nrun\n-d\n--name\n(canyon-build-[0-9a-f]{8})\n`).FindStringSubmatch(argv); started != nil && !strings.Contains(argv, "rm\n-f\n"+started[1]+"\n") {
				t.Fatalf("the holder %s was left behind:\n%s", started[1], argv)
			}
			if strings.Contains(c.docker, `"network inspect canyon-builds"`) && (strings.Contains(argv, "network\ncreate\n") || strings.Contains(argv, "network\nrm\n")) {
				t.Fatalf("a network of Canyon's name was changed, never only refused:\n%s", argv)
			}
		})
	}
}

// buildFloorCalls runs buildfloor.sh as the holder does, its iptables-nft, ip6tables-nft and sleep stand-ins
// writing each call to a log, the one named by fail exiting 1: the calls in order, and the script's error.
func buildFloorCalls(t *testing.T, fail string) ([]string, error) {
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
	cmd := exec.Command("sh", "-c", buildFloor)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "CANYON_BUILD_SUBNET=172.18.0.0/16", "CANYON_BUILD_HOST=10.0.0.4/32,203.0.113.7/32", "CANYON_BUILD_POOLS=172.28.0.0/16,10.213.0.0/16,10.214.0.0/16",
		"CANYON_BUILD_ALLOW=10.20.0.0/16:443,192.168.7.10/32:8081"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("buildfloor.sh: %s", out)
	}
	b, _ := os.ReadFile(calls) //nolint:gosec // the test's own temporary file
	return strings.Split(strings.TrimSpace(string(b)), "\n"), err
}

// The floor rejects every address of the Docker host's own (a public one among them, which public 80 and 443
// would otherwise return), link-local, the metadata addresses, the unroutable ranges, the apps pool and
// canyon-builds' own subnet before any CANYON_BUILD_ALLOW entry, so no entry opens them, and the private ranges after every
// entry; public TCP 80 and 443 return and the rest is rejected; nothing is forwarded; and the jump into its
// chain is its last rule, then sleep.
func TestTheBuildFloorRejectsWhatNoEntryOpens(t *testing.T) {
	calls, err := buildFloorCalls(t, "")
	if err != nil {
		t.Fatal(err)
	}
	reject := func(r string) string { return "iptables-nft -A CANYON-BUILD -d " + r + " -j REJECT" }
	allows := []string{"iptables-nft -A CANYON-BUILD -p tcp -d 10.20.0.0/16 --dport 443 -j RETURN", "iptables-nft -A CANYON-BUILD -p tcp -d 192.168.7.10/32 --dport 8081 -j RETURN"}
	first, last := slices.Index(calls, allows[0]), slices.Index(calls, allows[1])
	if first < 0 || last < first {
		t.Fatalf("the allowed entries, in order, are not rules:\n%s", strings.Join(calls, "\n"))
	}
	for _, r := range []string{"10.0.0.4/32", "203.0.113.7/32", "169.254.0.0/16", "168.63.129.16/32", "100.100.100.200/32", "0.0.0.0/8", "127.0.0.0/8", "224.0.0.0/4", "240.0.0.0/4", "172.18.0.0/16", "172.28.0.0/16", "10.213.0.0/16", "10.214.0.0/16"} {
		if at := slices.Index(calls, reject(r)); at < 0 || at > first {
			t.Errorf("%s is not rejected before the first allowed entry (call %d, the entry %d)", r, at, first)
		}
	}
	for _, r := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15"} {
		if at := slices.Index(calls, reject(r)); at < last {
			t.Errorf("%s is not rejected after the last allowed entry (call %d, the entry %d)", r, at, last)
		}
	}
	tail := []string{"iptables-nft -A CANYON-BUILD -p tcp --dport 80 -j RETURN", "iptables-nft -A CANYON-BUILD -p tcp --dport 443 -j RETURN", "iptables-nft -A CANYON-BUILD -j REJECT"}
	if at := slices.Index(calls, tail[0]); at < 0 || !slices.Equal(calls[at:at+3], tail) || !slices.Contains(calls, "iptables-nft -P FORWARD DROP") {
		t.Errorf("want public 80 and 443 returned, the rest rejected, and FORWARD dropped:\n%s", strings.Join(calls, "\n"))
	}
	if n := len(calls); n < 2 || calls[n-2] != "iptables-nft -I OUTPUT 1 -j CANYON-BUILD" || calls[n-1] != "sleep infinity" {
		t.Errorf("want the jump into CANYON-BUILD the last rule, then sleep:\n%s", strings.Join(calls, "\n"))
	}
	head := []string{"iptables-nft -N CANYON-BUILD", "iptables-nft -A CANYON-BUILD -o lo -j RETURN"}
	if len(calls) < 2 || !slices.Equal(calls[:2], head) {
		t.Errorf("want the chain's first rule the namespace's own loopback, where Docker's resolver is:\n%s", strings.Join(calls, "\n"))
	}
	// what it answers returns only after the host, Canyon's networks and the rest no entry opens are rejected
	if est, last := slices.Index(calls, "iptables-nft -A CANYON-BUILD -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN"), slices.Index(calls, reject("10.214.0.0/16")); est < last || est > first {
		t.Errorf("established connections return at call %d: want after the first rejects (the last %d) and before the first allowed entry (%d)\n%s", est, last, first, strings.Join(calls, "\n"))
	}
	if _, err := os.Stat("/proc/net/if_inet6"); err == nil { // where the namespace has IPv6, all of it is dropped but loopback, before the jump
		v6 := []string{"ip6tables-nft -N CANYON-BUILD", "ip6tables-nft -A CANYON-BUILD -o lo -j RETURN", "ip6tables-nft -A CANYON-BUILD -j DROP", "ip6tables-nft -I OUTPUT 1 -j CANYON-BUILD"}
		at := slices.Index(calls, v6[0])
		if at < 0 || at+4 > len(calls) || !slices.Equal(calls[at:at+4], v6) || at > slices.Index(calls, "iptables-nft -I OUTPUT 1 -j CANYON-BUILD") {
			t.Errorf("want IPv6 dropped but loopback before the jump:\n%s", strings.Join(calls, "\n"))
		}
	} else {
		t.Log("no /proc/net/if_inet6 here: the floor's IPv6 half is asserted on Linux")
	}
}

// A rule the kernel refuses stops the floor where it is: no jump into the chain, so the executor's check never
// finds it, and the holder never sleeps.
func TestTheBuildFloorStopsAtARefusedRule(t *testing.T) {
	calls, err := buildFloorCalls(t, "iptables-nft -P FORWARD DROP")
	if err == nil || slices.Contains(calls, "iptables-nft -I OUTPUT 1 -j CANYON-BUILD") || slices.Contains(calls, "sleep infinity") {
		t.Fatalf("a refused rule: %v, calls:\n%s", err, strings.Join(calls, "\n"))
	}
}

// A builder whose image runs as root is refused before pack runs: its NET_RAW would pass the floor. A uid is read
// as a number, 00 included; a name by the image's own id, and one it cannot read is refused too.
func TestBuildRefusesARootBuilder(t *testing.T) {
	for _, c := range []struct {
		user, id string // id: what the image's id -u answers, "!" when it fails
		says     string // "" builds
	}{
		{"", "", "runs its build as root"}, {"root", "0", "runs its build as root"}, {"0", "", "runs its build as root"},
		{"0:0", "", "runs its build as root"}, {"00", "", "runs its build as root"}, {"000:1001", "", "runs its build as root"},
		{"root:root", "0", "runs its build as root"}, {"+0", "0", "runs its build as root"}, {"builder", "0", "runs its build as root"},
		{"cnb", "!", "whose uid cannot be read"}, {"cnb", "", "whose uid cannot be read"},
		{"1001:1001", "", ""}, {"1001", "", ""}, {"cnb", "1000", ""},
	} {
		e, dir := fakes(t)
		id := `[ "$1 $2" = "run --rm" ] && case "$*" in *" -u") [ '` + c.id + `' = '!' ] && exit 1; echo '` + c.id + `'; exit 0;; esac`
		withDocker(t, e, `case "$*" in *"{{.Config.User}}"*) echo 'sha256:`+strings.Repeat("cd", 32)+` `+c.user+`'; exit 0;; esac
`+id)
		_, err := e.Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard)
		packed := readIf(dir, "pack-argv") != ""
		if c.says != "" && (err == nil || !strings.Contains(err.Error(), c.says) || packed) || c.says == "" && (err != nil || !packed) {
			t.Errorf("a builder running as %q (id -u %q): %v, pack ran %t, want %q", c.user, c.id, err, packed, c.says)
		}
	}
}

// A builder's user is read once per image: a second build on the same image runs no id -u, one on another does.
func TestBuildReadsABuildersUserOncePerImage(t *testing.T) {
	e, dir := fakes(t)
	withDocker(t, e, `case "$*" in *"{{.Config.User}}"*) echo "$(cat '`+dir+`/builder-id') cnb"; exit 0;; esac
[ "$1 $2" = "run --rm" ] && case "$*" in *" -u") echo 1000; exit 0;; esac`)
	build := func(id string) {
		must(t, os.WriteFile(filepath.Join(dir, "builder-id"), []byte(id), 0o600))
		_, err := e.Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard)
		must(t, err)
	}
	build("sha256:" + strings.Repeat("1", 64))
	build("sha256:" + strings.Repeat("1", 64))
	if n := strings.Count(readIf(dir, "argv"), "\nid\n"); n != 1 {
		t.Fatalf("one image's user read %d times, want once", n)
	}
	build("sha256:" + strings.Repeat("2", 64))
	if n := strings.Count(readIf(dir, "argv"), "\nid\n"); n != 2 {
		t.Fatalf("another image's user read %d times in all, want twice", n)
	}
}

// A refusal repeats: a root builder, and one whose user's uid cannot be read, is refused on every build on one
// executor, never cached as one that is not root; and an answer that is no image ID — none, or a tag — is never
// cached, so the next build reads the user again.
func TestBuildRefusesARootBuilderEveryTime(t *testing.T) {
	for _, c := range []struct{ id, user, idU string }{
		{"sha256:" + strings.Repeat("3", 64), "root", "0"},
		{"sha256:" + strings.Repeat("4", 64), "cnb", "!"},
	} {
		e, dir := fakes(t)
		withDocker(t, e, `case "$*" in *"{{.Config.User}}"*) echo '`+c.id+` `+c.user+`'; exit 0;; esac
[ "$1 $2" = "run --rm" ] && case "$*" in *" -u") [ '`+c.idU+`' = '!' ] && exit 1; echo '`+c.idU+`'; exit 0;; esac`)
		for range 2 {
			_, err := e.Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard)
			if err == nil || readIf(dir, "pack-argv") != "" {
				t.Fatalf("a builder running as %s (id -u %q), built again on one executor: %v, pack ran %t", c.user, c.idU, err, readIf(dir, "pack-argv") != "")
			}
		}
	}
	for _, id := range []string{"", "canyon-builder:1a2b3c"} {
		e, dir := fakes(t)
		withDocker(t, e, `case "$*" in *"{{.Config.User}}"*) echo '`+id+` cnb'; exit 0;; esac
[ "$1 $2" = "run --rm" ] && case "$*" in *" -u") echo 1000; exit 0;; esac`)
		for range 2 {
			_, err := e.Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard)
			must(t, err)
		}
		if n := strings.Count(readIf(dir, "argv"), "\nid\n"); n != 2 {
			t.Errorf("an image inspect answering %q for the ID: the user read %d times over two builds, want each time", id, n)
		}
	}
}

// Refused is the apps' pool and Canyon's own networks, each once: compose's apps pool is among its
// CANYON_EGRESS_POOLS too.
func TestRefusedNamesEachNetworkOnce(t *testing.T) {
	e := &Executor{Pool: netip.MustParsePrefix("10.213.0.0/16"), Canyon: []netip.Prefix{netip.MustParsePrefix("10.213.0.0/16"), netip.MustParsePrefix("10.214.0.0/16")}}
	if got := prefixes(e.Refused()); got != "10.213.0.0/16,10.214.0.0/16" {
		t.Fatalf("Refused: %s", got)
	}
	if got := prefixes((&Executor{}).Refused()); got != "172.28.0.0/16,10.213.0.0/16,10.214.0.0/16" {
		t.Fatalf("Refused unset: %s", got)
	}
}

// The builder's buildpacks as its metadata label names them, for project.toml's checks.
const carriedLabels = `{"io.buildpacks.builder.metadata":"{\"buildpacks\":[{\"id\":\"paketo-buildpacks/nodejs\",\"version\":\"7.1.0\",\"name\":\"Paketo Buildpack for Node.js\"},{\"id\":\"paketo-buildpacks/procfile\",\"version\":\"5.11.0\"}]}"}`

// A project.toml naming a buildpack pack would fetch — by its uri, from the buildpack registry, as an image, or at
// a version the builder does not carry — refuses the build before pack runs, and so does one that does not parse
// or carries a key Canyon does not read; the builder's own buildpacks by id, an inline script, build env, and a
// project.toml naming none build (contracts/executor.md, pack fetches nothing an app names).
func TestBuildRefusesAProjectTomlPackWouldFetch(t *testing.T) {
	fixture := map[string]string{}
	must(t, filepath.WalkDir("testdata/apps/project-uri", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // the test's fixture
		fixture[strings.TrimPrefix(path, "testdata/apps/project-uri/")] = string(b)
		return err
	}))
	const v02 = "[_]\nschema-version = \"0.2\"\n"
	for name, c := range map[string]struct {
		tree map[string]string
		says string // "" builds
	}{
		"the must-fail fixture":          {fixture, `buildpack "https://`},
		"a group buildpack's uri":        {map[string]string{"project.toml": v02 + "[[io.buildpacks.group]]\nuri = \"http://10.0.0.5:9000/bp.tgz\"\n"}, `buildpack "http://10.0.0.5:9000/bp.tgz" has a uri`},
		"a pre buildpack's image":        {map[string]string{"project.toml": v02 + "[[io.buildpacks.pre.group]]\nid = \"x\"\nuri = \"docker://10.0.0.5:5000/bp\"\n"}, "has a uri"},
		"a post buildpack's uri":         {map[string]string{"project.toml": v02 + "[[io.buildpacks.post.group]]\nuri = \"https://169.254.169.254/latest\"\n"}, "has a uri"},
		"an empty uri":                   {map[string]string{"project.toml": v02 + "[[io.buildpacks.group]]\nid = \"paketo-buildpacks/nodejs\"\nuri = \"\"\n"}, "has a uri"},
		"schema 0.1's buildpacks":        {map[string]string{"project.toml": "[[build.buildpacks]]\nuri = \"https://example.com/bp.tgz\"\n"}, "has a uri"},
		"schema 0.1's pre group":         {map[string]string{"project.toml": "[[build.pre.group]]\nuri = \"https://example.com/bp.tgz\"\n"}, "has a uri"},
		"schema 0.1's post group":        {map[string]string{"project.toml": "[[build.post.group]]\nid = \"acme/evil\"\nversion = \"1.0.0\"\n"}, `"acme/evil@1.0.0" is not one of the builder's`},
		"schema 0.3's group":             {map[string]string{"project.toml": "[_]\nschema-version = \"0.3\"\n[[io.buildpacks.group]]\nuri = \"https://example.com/bp.tgz\"\n"}, "has a uri"},
		"a registry buildpack":           {map[string]string{"project.toml": v02 + "[[io.buildpacks.group]]\nid = \"acme/evil\"\nversion = \"1.0.0\"\n"}, "is not one of the builder's"},
		"an image by id":                 {map[string]string{"project.toml": v02 + "[[io.buildpacks.group]]\nid = \"10.0.0.5:5000/evil/bp\"\n"}, "is not one of the builder's"},
		"another version":                {map[string]string{"project.toml": v02 + "[[io.buildpacks.group]]\nid = \"paketo-buildpacks/nodejs\"\nversion = \"6.0.0\"\n"}, `"paketo-buildpacks/nodejs@6.0.0" is not one of the builder's`},
		"no id":                          {map[string]string{"project.toml": v02 + "[[io.buildpacks.group]]\nversion = \"1.0.0\"\n"}, "is not one of the builder's"},
		"a script with no inline":        {map[string]string{"project.toml": v02 + "[[io.buildpacks.group]]\nid = \"paketo-buildpacks/nodejs\"\n[io.buildpacks.group.script]\napi = \"0.10\"\n"}, "no inline script"},
		"a key of 0.1 in its _":          {map[string]string{"project.toml": "[_]\nschema-version = \"0.1\"\nname = \"x\"\n"}, `_."name" is not a key Canyon reads`},
		"a key in io.buildpacks":         {map[string]string{"project.toml": v02 + "[io.buildpacks]\nexcluded = [\"docs\"]\n"}, `io.buildpacks."excluded" is not a key Canyon reads`},
		"exec-env":                       {map[string]string{"project.toml": "[_]\nschema-version = \"0.3\"\n[[io.buildpacks.build.env]]\nname = \"A\"\nvalue = \"b\"\nexec-env = [\"test\"]\n"}, `"exec-env" is not a key Canyon reads`},
		"an unknown schema":              {map[string]string{"project.toml": "[_]\nschema-version = \"0.9\"\n"}, `schema-version "0.9"`},
		"a version that is no string":    {map[string]string{"project.toml": v02 + "[[io.buildpacks.group]]\nid = \"paketo-buildpacks/nodejs\"\nversion = 7\n"}, "version is not a string"},
		"unparseable":                    {map[string]string{"project.toml": "[[io.buildpacks.group]\nid = \n"}, "project.toml: "},
		"no project.toml":                {map[string]string{"package.json": "{}"}, ""},
		"schema 0.1, said":               {map[string]string{"project.toml": "[_]\nschema-version = \"0.1\"\n[[build.buildpacks]]\nid = \"paketo-buildpacks/nodejs\"\n"}, ""},
		"another tool's tables":          {map[string]string{"project.toml": v02 + "[com.heroku.buildpacks]\nanything = \"x\"\n[io.other]\nURI = 1\n[metadata]\nlabel = \"y\"\n[[io.buildpacks.group]]\nid = \"paketo-buildpacks/nodejs\"\n"}, ""},
		"io beside schema 0.1":           {map[string]string{"project.toml": "[[io.buildpacks.group]]\nuri = \"https://example.com/bp.tgz\"\n"}, ""}, // pack's 0.1 reads no io, and neither does Canyon
		"build env alone":                {map[string]string{"project.toml": v02 + "[[io.buildpacks.build.env]]\nname = \"BP_NODE_RUN_SCRIPTS\"\nvalue = \"build\"\n"}, ""},
		"labels pack alone reads":        {map[string]string{"project.toml": "[_]\nschema-version = \"0.2\"\nid = \"app\"\nname = \"App\"\nversion = \"1\"\nauthors = [\"a\"]\n[_.metadata]\nanything = { at = \"all\" }\n"}, ""},
		"the builder's buildpack by id":  {map[string]string{"project.toml": v02 + "[[io.buildpacks.group]]\nid = \"paketo-buildpacks/nodejs\"\n[[io.buildpacks.post.group]]\nid = \"paketo-buildpacks/procfile\"\nversion = \"5.11.0\"\n"}, ""},
		"the builder's id, a tree path":  {map[string]string{"project.toml": v02 + "[[io.buildpacks.group]]\nid = \"paketo-buildpacks/nodejs\"\n", "paketo-buildpacks/nodejs/package.toml": "[[dependencies]]\nuri = \"http://10.0.0.5/bp.tgz\"\n"}, ""},
		"an inline script":               {map[string]string{"project.toml": v02 + "[[io.buildpacks.pre.group]]\nid = \"me/hello\"\n[io.buildpacks.pre.group.script]\napi = \"0.10\"\ninline = \"echo hello\"\n"}, ""},
		"schema 0.1's builder buildpack": {map[string]string{"project.toml": "[[build.buildpacks]]\nid = \"paketo-buildpacks/nodejs\"\nversion = \"7.1.0\"\n"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			e, dir := fakes(t)
			withDocker(t, e, `case "$*" in *"json .Config.Labels"*) echo '`+carriedLabels+`'; exit 0;; esac`)
			_, err := e.Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, c.tree)), io.Discard)
			packed := readIf(dir, "pack-argv") != ""
			if c.says == "" && (err != nil || !packed) || c.says != "" && (err == nil || !strings.HasPrefix(err.Error(), "project.toml: ") || !strings.Contains(err.Error(), c.says) || packed) {
				t.Fatalf("%v (pack ran %t), want %q", err, packed, c.says)
			}
		})
	}
}

// pack's TOML decoder is not Canyon's: it matches a key to a name in another case, or by Unicode folding (ſ for s,
// K for k), and of two keys that differ in case it takes either. pack reads Canyon's descriptor alone now, but a
// key spelled so is one pack's own reading would have taken as its name: Canyon refuses each such file rather than
// read it its own way and leave out what the author meant, and pack never runs.
func TestBuildRefusesAProjectTomlPackReadsOtherwise(t *testing.T) {
	const v02 = "[_]\nschema-version = \"0.2\"\n"
	const fetched = "uri = \"http://169.254.169.254/latest/meta-data/\"\n"
	const carried = "id = \"paketo-buildpacks/nodejs\"\n"
	for name, project := range map[string]string{
		"a table in another case":      v02 + "[[io.buildpacks.Group]]\n" + fetched + "[[io.buildpacks.group]]\n" + carried,
		"a key in another case":        v02 + "[[io.buildpacks.group]]\n" + carried + "URI = \"http://169.254.169.254/x\"\nuri = \"\"\n",
		"schema 0.1's two lists":       "[[build.Buildpacks]]\n" + fetched + "[[build.buildpacks]]\n" + carried,
		"pre in another case":          v02 + "[[io.buildpacks.Pre.group]]\n" + fetched + "[[io.buildpacks.pre.group]]\n" + carried,
		"io in another case":           v02 + "[[IO.buildpacks.group]]\n" + fetched + "[[io.buildpacks.group]]\n" + carried,
		"a long s for s":               v02 + "[[io.\"buildpack\u017F\".group]]\n" + fetched,
		"a Kelvin sign for k":          v02 + "[[io.\"buildpac\u212As\".group]]\n" + fetched,
		"a version pack reads, folded": v02 + "[[io.buildpacks.group]]\n" + carried + "\"ver\u017Fion\" = \"10.0.0.5:5000/bp\"\n",
		"a schema-version, folded":     "[_]\n\"Schema-Version\" = \"0.2\"\n[[io.buildpacks.group]]\n" + fetched,
		"schema 0.1's build, folded":   "[[Build.buildpacks]]\n" + fetched,
		"schema 0.1's project, folded": "[_]\nschema-version = \"0.1\"\n[Project]\nname = \"x\"\n[[build.buildpacks]]\n" + carried,
	} {
		t.Run(name, func(t *testing.T) {
			e, dir := fakes(t)
			withDocker(t, e, `case "$*" in *"json .Config.Labels"*) echo '`+carriedLabels+`'; exit 0;; esac`)
			_, err := e.Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"project.toml": project})), io.Discard)
			if packed := readIf(dir, "pack-argv"); err == nil || !strings.HasPrefix(err.Error(), "project.toml: ") || packed != "" {
				t.Fatalf("%q: %v, pack ran %t: want it refused before pack runs", project, err, packed != "")
			}
		})
	}
}

// pack reads the descriptor Canyon writes, never the app's project.toml: --descriptor names a file outside the
// tree, holding what Canyon read of the app's, byte for byte as here, in its own canonical keys, and nothing
// else: no builder, which pack would otherwise take over --builder's, and no label. A hostile string stays a
// string.
func TestPackReadsCanyonsDescriptorAlone(t *testing.T) {
	const hostile = "x'''\n[[io.buildpacks.group]]\nuri = \"http://169.254.169.254/\"\n" // a literal string's own end, then a table
	for name, c := range map[string]struct{ project, want string }{
		"schema 0.3": {
			"[_]\nschema-version = \"0.3\"\nname = \"App\"\n[io.buildpacks]\nexclude = [\"docs\"]\n[[io.buildpacks.group]]\nid = \"paketo-buildpacks/nodejs\"\n[[io.buildpacks.build.env]]\nname = \"BP_NODE_RUN_SCRIPTS\"\nvalue = \"build\"\n",
			"[_]\nschema-version = '0.2'\n\n[io]\n[io.buildpacks]\nexclude = ['docs']\n\n[[io.buildpacks.group]]\nid = 'paketo-buildpacks/nodejs'\n\n[io.buildpacks.build]\n[[io.buildpacks.build.env]]\nname = 'BP_NODE_RUN_SCRIPTS'\nvalue = 'build'\n",
		},
		"none": {"", "[_]\nschema-version = '0.2'\n\n[io]\n[io.buildpacks]\n"},
		"schema 0.1's env, include and groups": {
			"[project]\nname = \"App\"\n[build]\ninclude = [\"src\"]\nbuilder = \"evil/builder\"\n[[build.buildpacks]]\nid = \"paketo-buildpacks/nodejs\"\nversion = \"7.1.0\"\n[[build.env]]\nname = \"A\"\nvalue = \"1\"\n[[build.pre.group]]\nid = \"paketo-buildpacks/procfile\"\n",
			"[_]\nschema-version = '0.2'\n\n[io]\n[io.buildpacks]\ninclude = ['src']\n\n[[io.buildpacks.group]]\nid = 'paketo-buildpacks/nodejs'\nversion = '7.1.0'\n\n[io.buildpacks.build]\n[[io.buildpacks.build.env]]\nname = 'A'\nvalue = '1'\n\n[io.buildpacks.pre]\n[[io.buildpacks.pre.group]]\nid = 'paketo-buildpacks/procfile'\n",
		},
		"0.2's old env form, and a builder": {
			"[_]\nschema-version = \"0.2\"\n[io.buildpacks]\nbuilder = \"evil/builder\"\n[[io.buildpacks.env.build]]\nname = \"B\"\nvalue = \"2\"\n",
			"[_]\nschema-version = '0.2'\n\n[io]\n[io.buildpacks]\n[io.buildpacks.build]\n[[io.buildpacks.build.env]]\nname = 'B'\nvalue = '2'\n",
		},
		"pre and post, an inline script with its shell": {
			"[_]\nschema-version = \"0.2\"\n[[io.buildpacks.pre.group]]\nid = \"me/hello\"\n[io.buildpacks.pre.group.script]\napi = \"0.10\"\nshell = \"/bin/bash\"\ninline = \"echo hello\"\n[[io.buildpacks.post.group]]\nid = \"paketo-buildpacks/procfile\"\nversion = \"5.11.0\"\n",
			"[_]\nschema-version = '0.2'\n\n[io]\n[io.buildpacks]\n[io.buildpacks.pre]\n[[io.buildpacks.pre.group]]\nid = 'me/hello'\n\n[io.buildpacks.pre.group.script]\napi = '0.10'\ninline = 'echo hello'\nshell = '/bin/bash'\n\n[io.buildpacks.post]\n[[io.buildpacks.post.group]]\nid = 'paketo-buildpacks/procfile'\nversion = '5.11.0'\n",
		},
		"hostile strings": {
			"[_]\nschema-version = \"0.2\"\n[[io.buildpacks.group]]\nid = \"me/hello\"\n[io.buildpacks.group.script]\napi = \"0.10\"\ninline = \"\"\"" + hostile + "\"\"\"\n[[io.buildpacks.build.env]]\nname = \"C\"\nvalue = \"\"\"" + hostile + "\"\"\"\n",
			"", // read back below
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, dir := fakes(t)
			withDocker(t, e, `case "$*" in *"json .Config.Labels"*) echo '`+carriedLabels+`'; exit 0;; esac`)
			must(t, os.WriteFile(e.Pack, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+dir+"/pack-argv'\nwhile [ $# -gt 0 ]; do [ \"$1\" = --descriptor ] && cp \"$2\" '"+dir+"/descriptor'; shift; done\necho '===> BUILDING'\n"), 0o700)) //nolint:gosec // test fake
			tree := map[string]string{"package.json": "{}"}
			if c.project != "" {
				tree["project.toml"] = c.project
			}
			if _, err := e.Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, tree)), io.Discard); err != nil {
				t.Fatal(err)
			}
			argv := strings.Split(readIf(dir, "pack-argv"), "\n")
			at := slices.Index(argv, "--descriptor")
			if at < 0 || !filepath.IsAbs(argv[at+1]) || strings.Contains(argv[at+1], "canyon-build-") {
				t.Fatalf("pack is handed no descriptor of Canyon's outside the tree: %q", argv)
			}
			got := readIf(dir, "descriptor")
			if c.want == "" { // each hostile string reads back as the one string the app wrote, and the file holds one group, no uri
				var d written
				err := toml.Unmarshal([]byte(got), &d)
				if err != nil || len(d.IO.Buildpacks.Group) != 1 || d.IO.Buildpacks.Group[0].Script == nil || d.IO.Buildpacks.Group[0].Script.Inline != hostile ||
					d.IO.Buildpacks.Build == nil || len(d.IO.Buildpacks.Build.Env) != 1 || d.IO.Buildpacks.Build.Env[0].Value != hostile || strings.Count(got, "\n[[io.buildpacks.group]]\n") != 1 {
					t.Fatalf("the descriptor pack read:\n%s\n%v", got, err)
				}
			} else if got != c.want {
				t.Fatalf("the descriptor pack read:\n%s\nwant:\n%s", got, c.want)
			}
			if _, err := os.Stat(argv[at+1]); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the descriptor %s outlived its build: %v", argv[at+1], err)
			}
		})
	}
}

// CANYON_BUILD_ALLOW and CANYON_APPS_ALLOW take IPv4 prefixes at /8 or narrower, each with one TCP port, and refuse
// the executor's start on any other entry, naming the setting — and on any that overlaps link-local, a metadata
// address, the unroutable ranges or Canyon's own networks (the apps' pool, where every app network's gateway is,
// and CANYON_EGRESS_POOLS), which no entry may open.
func TestAllowSettingsRefuseWhatNothingCanyonRunsMayReach(t *testing.T) {
	for _, name := range []string{"CANYON_BUILD_ALLOW", "CANYON_APPS_ALLOW"} {
		allowSettingRefuses(t, name)
	}
}

func allowSettingRefuses(t *testing.T, name string) {
	t.Helper()
	canyon := []netip.Prefix{netip.MustParsePrefix("172.28.0.0/16"), netip.MustParsePrefix("10.213.0.0/16"), netip.MustParsePrefix("10.214.0.0/16")}
	allow := func(v string) ([]string, error) {
		return AllowSetting(func(k string) string {
			if k == name {
				return v
			}
			return "10.30.0.0/16:443" // the other setting's, which this one never reads
		}, name, canyon...)
	}
	if got, err := allow(""); err != nil || got != nil {
		t.Fatalf("unset: %v %v", got, err)
	}
	if got, err := allow("10.20.0.0/16:443,192.168.7.10/32:8081,10.215.0.0/16:22"); err != nil || !slices.Equal(got, []string{"10.20.0.0/16:443", "192.168.7.10/32:8081", "10.215.0.0/16:22"}) {
		t.Fatalf("three entries: %v %v", got, err)
	}
	for _, entry := range []string{
		"10.0.0.0/8:5432", "10.214.2.0/24:7778", "10.214.1.3/32:5432", "172.28.0.0/16:443", "172.16.0.0/12:443", "10.213.4.0/24:3000",
		"169.254.169.254/32:80", "169.254.0.0/16:443", "169.0.0.0/8:443", "168.63.129.16/32:80", "168.63.0.0/16:443",
		"100.100.100.200/32:80", "100.64.0.0/10:443", "0.0.0.0/0:443", "0.0.0.0/8:443", "127.0.0.1/32:80", "224.0.0.0/8:80",
		"240.0.0.0/8:443", "8.0.0.0/7:443", "10.0.0.1/8:443", "10.0.0.0/8", "10.0.0.0/8:0", "10.0.0.0/8:65536",
		"10.0.0.0/8:80-90", "10.0.0.0/8:*", "10.0.0.0/8:+80", "10.0.0.0/8:080", "fd00::/8:443", "10.0.0.0/8:443,",
		" 10.0.0.0/8:443", "registry.corp.example:443", "10.0.0.5:443",
	} {
		if got, err := allow(entry); err == nil || !strings.HasPrefix(err.Error(), name+": ") {
			t.Errorf("%s=%q: %v %v, want it refused", name, entry, got, err)
		}
	}
}

// A build value pack's own process would read — a proxy, in either case, a certificate file, GODEBUG, where
// registry credentials are read from, any PACK_ name — or any DOCKER_ name, in any case, refuses the build before pack runs: pack
// travels outside the build's floor, and would connect where the app said. A runtime value may set a proxy, which
// the app reads, and never a DOCKER_ name, which the docker child would.
func TestBuildRefusesAValuePackWouldRead(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "https_proxy", "All_Proxy", "NO_PROXY", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR", "GODEBUG",
		"REGISTRY_AUTH_FILE", "xdg_runtime_dir", "DOCKER_CONTEXT", "docker_default_platform", "Docker_Api_Version", "DOCKER_CONTENT_TRUST",
		"PACK_CACHE_IMAGE", "pack_home", "Pack_Anything"} {
		e, dir := fakes(t)
		_, err := e.Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f", BuildValues: map[string]string{name: "http://10.0.0.5:7778"}}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard)
		if packed := readIf(dir, "pack-argv"); err == nil || !strings.Contains(err.Error(), `env "`+name+`"`) || packed != "" {
			t.Errorf("build value %s: %v, pack ran %t", name, err, packed != "")
		}
	}
	e, dir := fakes(t)
	if _, err := e.Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f", BuildValues: map[string]string{"NEXT_PUBLIC_PROXY_URL": "x", "DOCKERFILE_PATH": "y"}}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard); err != nil || readIf(dir, "pack-argv") == "" {
		t.Errorf("values named otherwise: %v", err)
	}
	run := func(env map[string]string) error {
		e, _ := fakes(t)
		_, err := e.Run(t.Context(), RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Env: env, Network: "canyon-app-claims-portal"})
		return err
	}
	if err := run(map[string]string{"HTTPS_PROXY": "http://proxy.corp:3128", "SSL_CERT_FILE": "/etc/ca.pem", "PACK_SIZE": "12"}); err != nil {
		t.Errorf("a runtime proxy, certificate file and PACK_ name, the app's own: %v", err)
	}
	if err := run(map[string]string{"docker_context": "elsewhere"}); err == nil || !strings.Contains(err.Error(), `env "docker_context"`) {
		t.Errorf("a runtime DOCKER_ name: %v", err)
	}
}
