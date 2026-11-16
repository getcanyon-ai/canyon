package egress_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/egresscontrol"
)

// service is the part of a compose service these tests read.
type service struct {
	Image       string            `yaml:"image"`
	NetworkMode string            `yaml:"network_mode"`
	Command     []string          `yaml:"command"`
	Entrypoint  []string          `yaml:"entrypoint"`
	Environment []string          `yaml:"environment"`
	ExtraHosts  []string          `yaml:"extra_hosts"`
	Labels      map[string]string `yaml:"labels"`
	Volumes     []string          `yaml:"volumes"`
	Tmpfs       []string          `yaml:"tmpfs"`
	User        string            `yaml:"user"`
	CapDrop     []string          `yaml:"cap_drop"`
	CapAdd      []string          `yaml:"cap_add"`
	ReadOnly    bool              `yaml:"read_only"`
	DependsOn   dependsOn         `yaml:"depends_on"`
	Healthcheck struct {
		Test []string `yaml:"test"`
	} `yaml:"healthcheck"`
}

// dependsOn is a service's depends_on, either form compose takes: each service by name, its condition ("" in the
// list form).
type dependsOn map[string]string

func (d *dependsOn) UnmarshalYAML(n *yaml.Node) error {
	*d = dependsOn{}
	if n.Kind == yaml.SequenceNode {
		var names []string
		if err := n.Decode(&names); err != nil {
			return err
		}
		for _, name := range names {
			(*d)[name] = ""
		}
		return nil
	}
	var m map[string]struct {
		Condition string `yaml:"condition"`
	}
	if err := n.Decode(&m); err != nil {
		return err
	}
	for name, c := range m {
		(*d)[name] = c.Condition
	}
	return nil
}

// Envoy's floor refuses the Docker host's own addresses (#2524), which egress-host lists from the host's network
// namespace and nothing else of it: the port check's busybox, pinned as the executor pins it, every capability
// dropped, a read-only root, writing `ip addr show` into a volume the holder reads, read-only, at its start; and the
// holder starts only once that listing is there, its healthcheck, so the floor never starts without it.
func TestEgressHostListsTheHostsAddressesForEnvoysFloor(t *testing.T) {
	all := services(t, "docker-compose.yml")
	h, holder := all["egress-host"], all["egress"]
	pin, ok := strings.CutPrefix(h.Image, "${CANYON_IMAGE_BUSYBOX:-")
	if !ok || strings.TrimSuffix(pin, "}") != "docker.io/library/busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e" {
		t.Errorf("egress-host's image is %q: want the port check's busybox, CANYON_IMAGE_BUSYBOX's pin", h.Image)
	}
	if h.NetworkMode != "host" || !slices.Equal(h.CapDrop, []string{"ALL"}) || len(h.CapAdd) > 0 || !h.ReadOnly {
		t.Errorf("egress-host: network_mode %q, cap_drop %v, cap_add %v, read_only %t; want the host's namespace, no capability, a read-only root",
			h.NetworkMode, h.CapDrop, h.CapAdd, h.ReadOnly)
	}
	if !slices.Contains(h.Volumes, "canyon-egress-host:/run/canyon-host") || !slices.Contains(holder.Volumes, "canyon-egress-host:/run/canyon-host:ro") {
		t.Errorf("egress-host's volumes %v, the holder's %v: want the listing's volume, read-only to the holder", h.Volumes, holder.Volumes)
	}
	// a listing of this boot or none: the last boot's goes first, and the kernel's boot_id is written beside the addresses
	script := strings.Join(h.Entrypoint, " ")
	rm, boot := strings.Index(script, "rm -f /run/canyon-host/addrs"), strings.Index(script, "/proc/sys/kernel/random/boot_id")
	if addrs, end := strings.Index(script, "ip addr show"), strings.Index(script, "echo end"); rm < 0 || boot < rm || addrs < boot || end < addrs {
		t.Errorf("egress-host's entrypoint %q: want the listing removed first, then the boot_id, ip addr show and end written", script)
	}
	if v, _ := holder.env("CANYON_EGRESS_HOST_FILE"); v != "/run/canyon-host/addrs" || holder.DependsOn["egress-host"] != "service_healthy" {
		t.Errorf("the holder reads the listing at %q, after egress-host is %q; want /run/canyon-host/addrs, once healthy", v, holder.DependsOn["egress-host"])
	}
}

// services is the compose file at path, from the repository's root, its anchors resolved.
func services(t *testing.T, path string) map[string]service {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", path)) //nolint:gosec // a fixed path in this repository
	if err != nil {
		t.Fatal(err)
	}
	var f struct{ Services map[string]service }
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f.Services
}

func (s service) env(key string) (string, bool) {
	for _, kv := range s.Environment {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// docker compose up recreates a container when its service's config hash moves, and the hash reads a config's name,
// never its contents: a change to the bootstrap alone would leave a running Envoy on the old file after an upgrade. So
// the service carries their sum in a label, as Kubernetes' pod template does (deploy/k8s/egress_test.go), and the hash
// moves with it.
func TestComposeRollsEnvoyWhenItsConfigChanges(t *testing.T) {
	var whole []byte
	for _, f := range []string{"envoy.sh", "envoy.yaml"} {
		raw, err := os.ReadFile(f) //nolint:gosec // a fixed path in this repository
		if err != nil {
			t.Fatal(err)
		}
		whole = append(whole, raw...)
	}
	sum := sha256.Sum256(whole)
	if want, got := hex.EncodeToString(sum[:]), services(t, "docker-compose.yml")["egress-envoy"].Labels["canyon.egress-config-sha256"]; got != want {
		t.Errorf("egress-envoy has canyon.egress-config-sha256 %q, its config's is %q: set it, or an upgrade never reaches the running Envoy", got, want)
	}
}

// The names e2e/egress_test.go calls its stand-ins by live in the e2e overlay alone, on the holder, whose
// /etc/hosts Envoy shares: the shipped compose file names no host for the gateway's namespace, where a name a
// file pins would be one an app could call past the resolvers an operator chose.
func TestShippedComposeNamesNoHostForTheGateway(t *testing.T) {
	for name, s := range services(t, "docker-compose.yml") {
		if (name == "egress" || name == "egress-envoy" || name == "egress-control") && len(s.ExtraHosts) > 0 {
			t.Errorf("the shipped %s names hosts %v: the stand-ins' names are the e2e overlay's alone", name, s.ExtraHosts)
		}
	}
	hosts := services(t, "e2e/compose.e2e.yml")["egress"].ExtraHosts
	stray := func(h string) bool {
		name, _, _ := strings.Cut(h, ":")
		return !strings.HasSuffix(name, ".canyon-floor.test")
	}
	if len(hosts) == 0 || slices.ContainsFunc(hosts, stray) {
		t.Errorf("the e2e overlay's holder names %v; want the stand-ins' names, each under canyon-floor.test", hosts)
	}
}

// egress-control runs in the holder's namespace beside Envoy (design/EGRESS.md §3.1): healthy once it holds a
// policy, its probe; pulling the server's policy at the receiver's address, port 7782, where the server serves it
// and the holder's floor admits it, neither naming it otherwise; one replica, the most that answer at once;
// answering Envoy at the loopback address Envoy, the floor and egress-control all default to, none naming another;
// holding the receiver's token the server and Envoy hold, which its interception listener's access log sends (#2740);
// and serving no metrics, whose listener would sit on every app network the holder joins.
func TestComposeRunsEgressControlBesideEnvoy(t *testing.T) {
	all := services(t, "docker-compose.yml")
	c, server, holder, envoy := all["egress-control"], all["server"], all["egress"], all["egress-envoy"]
	if c.NetworkMode != "service:egress" || !slices.Equal(c.Command, []string{"egress-control"}) ||
		!slices.Equal(c.Healthcheck.Test, []string{"CMD", "/canyon-server", "egress-control", "probe"}) {
		t.Errorf("egress-control runs %v in %q, healthy by %v; want egress-control in the holder's namespace, healthy by its probe", c.Command, c.NetworkMode, c.Healthcheck.Test)
	}
	receiver, _ := server.env("CANYON_EGRESS_RECEIVER_ADDR")
	ip, _, _ := strings.Cut(receiver, ":")
	if url, _ := c.env("CANYON_EGRESS_POLICY_URL"); ip == "" || url != "http://"+ip+":7782/egress/policy" {
		t.Errorf("egress-control pulls %q; want the server's policy, http://%s:7782/egress/policy", url, ip)
	}
	for name, s := range map[string]service{"server": server, "egress": holder} {
		if v, ok := s.env("CANYON_EGRESS_POLICY_ADDR"); ok && v != ip+":7782" {
			t.Errorf("%s's CANYON_EGRESS_POLICY_ADDR is %q, not the receiver's address on 7782", name, v)
		}
	}
	if n, _ := c.env("CANYON_EGRESS_REPLICAS"); n != "1" {
		t.Errorf("egress-control's CANYON_EGRESS_REPLICAS is %q; want 1, the one replica", n)
	}
	for name, s := range map[string]service{"egress": holder, "egress-envoy": envoy, "egress-control": c} {
		if v, ok := s.env("CANYON_EGRESS_CONTROL_ADDR"); ok {
			t.Errorf("%s names CANYON_EGRESS_CONTROL_ADDR %q: Envoy, the floor and egress-control share its default alone", name, v)
		}
	}
	if v, ok := c.env("CANYON_EGRESS_CONTROL_METRICS_ADDR"); ok {
		t.Errorf("egress-control serves metrics on %q in the shipped compose file", v)
	}
	token, _ := server.env("CANYON_EGRESS_RECEIVER_TOKEN")
	for name, s := range map[string]service{"egress-envoy": envoy, "egress-control": c} {
		if v, _ := s.env("CANYON_EGRESS_RECEIVER_TOKEN"); token == "" || v != token {
			t.Errorf("%s's CANYON_EGRESS_RECEIVER_TOKEN is %q, the server's %q; want the one token", name, v, token)
		}
	}
}

// Envoy's admin interface is a Unix socket two containers can reach and nothing else can (design/EGRESS.md §2.1,
// #2374): it can stop Envoy and read its config, so no app, and no other service of Canyon's, mounts its volume in
// any compose file: whoever reaches it reads the bootstrap, the receiver's token in it, and POSTs admin calls. The
// volume is in memory and holds the socket alone, Envoy's own uid's, as both run as, so its mode 0600 admits the two.
// egress-control mounts it read-only and mounts nothing else of Envoy's: the socket is all it reads.
func TestEnvoysAdminSocketIsEnvoysAndEgressControlsAlone(t *testing.T) {
	const volume = "canyon-egress-admin"
	dir := filepath.Dir(egresscontrol.AdminSocket)
	for _, file := range []string{"docker-compose.yml", "e2e/compose.e2e.yml", "deploy/docker/team.yml"} {
		all := services(t, file)
		var mounted []string
		for name, s := range all {
			if slices.ContainsFunc(s.Volumes, func(v string) bool { return strings.HasPrefix(v, volume+":") }) {
				mounted = append(mounted, name)
			}
		}
		slices.Sort(mounted)
		if want := []string{"egress-control", "egress-envoy"}; file == "docker-compose.yml" && !slices.Equal(mounted, want) {
			t.Errorf("%s mounts %s on %v; want %v and nothing else", file, volume, mounted, want)
		} else if file != "docker-compose.yml" && len(mounted) != 0 {
			t.Errorf("%s adds %s to %v: only the shipped file's two mount it", file, volume, mounted)
		}
	}
	all := services(t, "docker-compose.yml")
	envoy, control := all["egress-envoy"], all["egress-control"]
	if !slices.Contains(envoy.Volumes, volume+":"+dir) || !slices.Contains(control.Volumes, volume+":"+dir+":ro") {
		t.Errorf("Envoy mounts %v and egress-control %v; want %s at %s, read-only to egress-control", envoy.Volumes, control.Volumes, volume, dir)
	}
	if envoy.User != "65534:65534" || control.User != envoy.User {
		t.Errorf("Envoy runs as %q and egress-control as %q: one uid, or the socket's mode 0600 shuts one out", envoy.User, control.User)
	}
	if len(control.Tmpfs) != 0 || slices.ContainsFunc(envoy.Tmpfs, func(m string) bool { return strings.HasPrefix(m, dir) }) {
		t.Errorf("egress-control mounts tmpfs %v, Envoy %v: the socket is on the shared volume, and is all egress-control mounts of Envoy's", control.Tmpfs, envoy.Tmpfs)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Volumes map[string]struct {
			Name       string            `yaml:"name"`
			DriverOpts map[string]string `yaml:"driver_opts"`
		} `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	v := f.Volumes[volume]
	if v.Name != volume || v.DriverOpts["type"] != "tmpfs" || !strings.Contains(v.DriverOpts["o"], "uid=65534,gid=65534,mode=0700") {
		t.Errorf("the volume is %+v; want %s by that name, in memory, Envoy's uid's alone", v, volume)
	}
}

// Each build's holder runs the egress gateway's holder image (contracts/executor.md, Images and database
// sources): the executor's pin, which a stock compose install leaves unset, is compose's own, so a bump of one
// that leaves the other behind fails here; and a build's floor refuses the same networks of Canyon's as egress's.
func TestTheBuildsHolderIsEgress(t *testing.T) {
	all := services(t, "docker-compose.yml")
	pin, ok := strings.CutPrefix(all["egress"].Image, "${CANYON_IMAGE_EGRESS_HOLDER:-")
	if !ok || !strings.HasSuffix(pin, "}") || strings.TrimSuffix(pin, "}") != executor.HolderImage {
		t.Errorf("compose's egress holder is %q; the executor's pin %q", all["egress"].Image, executor.HolderImage)
	}
	if v, ok := all["executor"].env("CANYON_IMAGE_EGRESS_HOLDER"); !ok || v != "${CANYON_IMAGE_EGRESS_HOLDER:-}" {
		t.Errorf("compose hands the executor CANYON_IMAGE_EGRESS_HOLDER as %q, not egress's own setting", v)
	}
	ours, _ := all["executor"].env("CANYON_EGRESS_POOLS")
	if theirs, ok := all["egress"].env("CANYON_EGRESS_POOLS"); !ok || ours != theirs {
		t.Errorf("a build's floor refuses Canyon's networks %q; egress's %q", ours, theirs)
	}
	for _, setting := range []string{"CANYON_BUILD_ALLOW", "CANYON_APPS_ALLOW"} { // .env's, empty unless the operator names an entry
		if v, ok := all["executor"].env(setting); !ok || v != "${"+setting+":-}" {
			t.Errorf("compose hands the executor %s as %q, not .env's own", setting, v)
		}
	}
}
