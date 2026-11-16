package executor

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/getcanyon-ai/canyon/internal/subjecthash"
)

// ErrInternal is a /run without egress on an app network that is internal: the app would reach nothing
// outside it, so it is refused where a person sees it, 409, never started cut off (contracts/executor.md, Egress).
var ErrInternal = errors.New("internal network")

// ErrOpen is a /run carrying egress on an app network that is not internal, which Docker cannot make so:
// the app would reach past Canyon's egress gateway, so it is refused, 409, never started unfloored, until
// the network is recreated (contracts/executor.md, Egress; #2254).
var ErrOpen = errors.New("open network")

// executor.schema.json's shapes of a /run's run and egress (contracts/executor.md, Egress).
var (
	runRe   = regexp.MustCompile(`^gr_[a-z0-9]{4,}$`)
	proxyRe = regexp.MustCompile(`^http://([a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?):3128$`)
	hostRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

// proxyPort is Envoy's listener on every host (design/EGRESS.md §2.1).
const proxyPort = "3128"

// maxNoProxy bounds a /run's egress.no_proxy.
const maxNoProxy = 32

// proxyVars are what the executor sets with egress and env may not name; JDK_JAVA_OPTIONS it appends to.
var proxyVars = []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "NO_PROXY", "no_proxy", "NODE_USE_ENV_PROXY"}

// ProxyEnv is what the app's environment holds under egress, set over env: the proxy variables to Envoy,
// NO_PROXY to every in-network host the executor or the server wrote — the database, the key set's
// host, the loopback — each once, so the app's token check and its database never reach Envoy, and the
// JVM's proxy properties appended to any JDK_JAVA_OPTIONS env gives (contracts/executor.md, Egress).
// env naming a variable it sets, or an egress out of the contract's shape, refuses the call.
func ProxyEnv(egress *Egress, jwksURL string, env map[string]string) (map[string]string, error) {
	return ProxyEnvTo(egress.Proxy, egress, jwksURL, env)
}

// ProxyEnvTo is ProxyEnv with the variables naming proxy, http://<host>:<port>, in place of egress.proxy: on
// Kubernetes from layer 3 the pod's own forwarder, which relays to egress.proxy alone (contracts/executor.md,
// On Kubernetes). egress is checked as ProxyEnv checks it.
func ProxyEnvTo(proxy string, egress *Egress, jwksURL string, env map[string]string) (map[string]string, error) {
	if !proxyRe.MatchString(egress.Proxy) || len(egress.NoProxy) > maxNoProxy {
		return nil, fmt.Errorf("%w: egress.proxy is http://<host>:%s and egress.no_proxy at most %d hosts", ErrRequest, proxyPort, maxNoProxy)
	}
	for i, h := range egress.NoProxy {
		if !hostRe.MatchString(h) || slices.Contains(egress.NoProxy[:i], h) {
			return nil, fmt.Errorf("%w: egress.no_proxy[%d] is not a host name or an IPv4 address named once", ErrRequest, i)
		}
	}
	if err := CheckCA(egress.CA); err != nil {
		return nil, err
	}
	for _, k := range proxyVars {
		if _, ok := env[k]; ok {
			return nil, fmt.Errorf("%w: env %s is the executor's under egress", ErrRequest, k)
		}
	}
	direct := slices.Clone(egress.NoProxy)
	if u, err := url.Parse(jwksURL); err == nil && u.Hostname() != "" {
		direct = append(direct, strings.ToLower(u.Hostname()))
	}
	var once []string
	for _, h := range append(direct, "localhost", "127.0.0.1") {
		if !slices.Contains(once, h) {
			once = append(once, h)
		}
	}
	via, err := url.Parse(proxy)
	if err != nil || via.Scheme != "http" || via.Hostname() == "" || via.Port() == "" {
		return nil, fmt.Errorf("the proxy %q is not http://<host>:<port>", proxy)
	}
	host, port, noProxy := via.Hostname(), via.Port(), strings.Join(once, ",")
	jvm := "-Dhttps.proxyHost=" + host + " -Dhttps.proxyPort=" + port + " -Dhttp.proxyHost=" + host + " -Dhttp.proxyPort=" + port +
		" -Dhttp.nonProxyHosts=" + strings.Join(once, "|")
	if given := strings.TrimSpace(env["JDK_JAVA_OPTIONS"]); given != "" {
		jvm = given + " " + jvm
	}
	return map[string]string{
		"HTTPS_PROXY": proxy, "HTTP_PROXY": proxy, "https_proxy": proxy, "http_proxy": proxy,
		"NO_PROXY": noProxy, "no_proxy": noProxy, "NODE_USE_ENV_PROXY": "1", "JDK_JAVA_OPTIONS": jvm,
	}, nil
}

// TrustDir is where a /run's egress.ca lands in the app container, read-only (contracts/executor.md, egress.ca).
const TrustDir = "/etc/canyon-egress"

// SystemRoots is the system roots of the executor's own image, read at each /run with egress.ca: the first half
// of system-and-canyon.pem.
const SystemRoots = "/etc/ssl/certs/ca-certificates.crt"

// trustVars are what egress.ca sets in the app's environment, unless env names one of them.
var trustVars = []string{"SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE"}

// maxRoots bounds egress.ca; maxRootPEM each of its roots.
const (
	maxRoots   = 8
	maxRootPEM = 8 << 10
)

var rootNameRe = regexp.MustCompile(`^([0-9a-f]{8})\.[0-9]$`)

// CheckCA refuses an egress.ca out of the contract's shapes, ErrRequest: one to 8 roots, each one CA certificate,
// PEM, at most 8 KiB and nothing else, under a name of its own that is its OpenSSL subject hash and an index.
func CheckCA(roots []Root) error {
	if len(roots) > maxRoots || roots != nil && len(roots) == 0 { // nil is no egress.ca; [] is one out of shape
		return fmt.Errorf("%w: egress.ca is one to %d roots", ErrRequest, maxRoots)
	}
	seen := map[string]bool{}
	for i, r := range roots {
		m := rootNameRe.FindStringSubmatch(r.Name)
		block, rest := pem.Decode([]byte(r.PEM))
		switch {
		case m == nil || seen[r.Name]:
			return fmt.Errorf("%w: egress.ca[%d]'s name is not <8 hex>.<n> named once", ErrRequest, i)
		case len(r.PEM) > maxRootPEM || block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0:
			return fmt.Errorf("%w: egress.ca[%d]'s pem is not one certificate, PEM, at most %d bytes", ErrRequest, i, maxRootPEM)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.BasicConstraintsValid || !cert.IsCA {
			return fmt.Errorf("%w: egress.ca[%d]'s pem is not a CA certificate", ErrRequest, i)
		}
		if hash, err := subjecthash.Of(cert.RawSubject); err != nil || hash != m[1] {
			return fmt.Errorf("%w: egress.ca[%d]'s name is not its certificate's subject hash, which OpenSSL and the JVM's loader look it up by", ErrRequest, i)
		}
		seen[r.Name] = true
	}
	return nil
}

// TrustFiles is what lands in TrustDir for roots, checked by CheckCA: each root under its name, bundle.pem the roots
// in their order, and system-and-canyon.pem system, the executor image's own roots, then the same roots. system with
// no certificate in it fails the start: never a system-and-canyon.pem of Canyon's roots alone.
func TrustFiles(roots []Root, system []byte) (map[string][]byte, error) {
	if pool := x509.NewCertPool(); !pool.AppendCertsFromPEM(system) {
		return nil, fmt.Errorf("%s holds no certificate: the app would trust Canyon's roots alone", SystemRoots)
	}
	files := map[string][]byte{}
	var bundle []byte
	for _, r := range roots {
		files[r.Name] = []byte(r.PEM)
		bundle = append(bundle, r.PEM...)
	}
	files["bundle.pem"] = bundle
	both := bytes.TrimRight(system, "\n")
	files["system-and-canyon.pem"] = append(append(both, '\n'), bundle...)
	return files, nil
}

// TrustEnv is what egress.ca sets in the app's environment: the three variables naming TrustDir, or none when env
// names any of them, whose own values stand (the server then records the /run as carrying no root).
func TrustEnv(env map[string]string) map[string]string {
	for _, k := range trustVars {
		if _, ok := env[k]; ok {
			return nil
		}
	}
	return map[string]string{
		"SSL_CERT_DIR":        "/etc/ssl/certs:" + TrustDir,
		"NODE_EXTRA_CA_CERTS": TrustDir + "/bundle.pem",
		"REQUESTS_CA_BUNDLE":  TrustDir + "/system-and-canyon.pem",
	}
}

// ServiceAccount is app target's own ServiceAccount on Kubernetes from layer 3, whose projected token proves its
// floored pods to Canyon's egress gateway (design/EGRESS.md §4.3): the executor's name for it, which the server's
// policy names too.
func ServiceAccount(app, target string) string { return "canyon-app-" + app + "-" + target }

// slots are a target's two fixed addresses in an internal app network's /24, its lower /25: a redeploy
// takes the one its previous container does not hold (design/EGRESS.md §2.3).
var slots = map[string][2]byte{"preview": {10, 12}, "prod": {11, 13}}

// appNet is what docker network inspect says of an app network.
type appNet struct {
	Name     string `json:"Name"`
	Internal bool   `json:"Internal"`
	IPAM     struct {
		Config []ipamConfig `json:"Config"`
	} `json:"IPAM"`
	Containers map[string]endpoint `json:"Containers"` // by container id
}

type ipamConfig struct {
	Subnet  string `json:"Subnet"`
	Gateway string `json:"Gateway"`
}

type endpoint struct {
	Name        string `json:"Name"`
	IPv4Address string `json:"IPv4Address"` // the address and its prefix length
}

func (n appNet) members() []string {
	var names []string
	for _, c := range n.Containers {
		names = append(names, c.Name)
	}
	return names
}

// subnet is the network's IPv4 /24, invalid on any other: the executor cuts every app network a /24.
func (n appNet) subnet() netip.Prefix {
	for _, c := range n.IPAM.Config {
		if p, err := netip.ParsePrefix(c.Subnet); err == nil && p.Addr().Is4() && p.Bits() == 24 {
			return p.Masked()
		}
	}
	return netip.Prefix{}
}

// at is the address host of the /24 holds; invalid without one.
func at(subnet netip.Prefix, host byte) netip.Addr {
	if !subnet.IsValid() {
		return netip.Addr{}
	}
	a := subnet.Addr().As4()
	a[3] = host
	return netip.AddrFrom4(a)
}

// slot is the fixed address name's new container starts at on the internal network n: the first of its
// target's two that the container a redeploy sets aside does not hold — name itself, or the -previous a
// crash left, which the redeploy names back first (Run).
func slot(n appNet, name, target string) (string, error) {
	sub := n.subnet()
	if !sub.IsValid() {
		return "", fmt.Errorf("network %s is not a /24: no fixed address on it", n.Name)
	}
	aside := name
	if !slices.Contains(n.members(), name) {
		aside = name + "-previous"
	}
	var asideAt netip.Addr
	for _, c := range n.Containers {
		if p, err := netip.ParsePrefix(c.IPv4Address); err == nil && c.Name == aside {
			asideAt = p.Addr()
		}
	}
	if first := at(sub, slots[target][0]); first != asideAt {
		return first.String(), nil
	}
	return at(sub, slots[target][1]).String(), nil
}

// RecreateNetwork makes the app's network what a /run under the host's via would make it now, either way
// (contracts/executor.md, POST /networks/recreate): the app's containers the daemon lists stopped and left
// so, every container still on the network disconnected, the network removed and created again on its own
// /24 — internal when internal is true, open when false — and the joined containers, and on an internal
// network the door, connected again. It starts nothing: a container started without the proxy variables
// would reach nothing on an internal network, one started with them would reach past Envoy on an open one,
// so the server /runs each env again. What fails after the stop leaves the app stopped, and the same call
// again repeats the whole of it. Under e.nets throughout, as a /run's start onto the network is.
func (e *Executor) RecreateNetwork(ctx context.Context, app string, internal bool) error {
	if !validApp(app) {
		return fmt.Errorf("%w: app must match %s", ErrRequest, appPattern)
	}
	e.nets.Lock()
	defer e.nets.Unlock()
	ctx, cancel := context.WithTimeout(ctx, netDeadline)
	defer cancel()
	names := []string{app + "-preview", app + "-prod", app + "-preview-previous", app + "-prod-previous"}
	listed, err := e.containers(ctx, names...)
	if err != nil {
		return err
	}
	names = slices.DeleteFunc(names, func(n string) bool { _, ok := listed[n]; return !ok })
	if len(names) > 0 {
		if out, err := child(ctx, e.docker(), append([]string{"stop"}, names...)...).CombinedOutput(); err != nil {
			log.Printf("executor: stop %v: %s", names, strings.TrimSpace(string(out)))
			return fmt.Errorf("docker stop: %w", err)
		}
	}
	holders, err := e.holders(ctx, app) // every one: the apps that ran in them are stopped for good, and the next /run brings its own
	if err != nil {
		return err
	}
	e.removeHolders(ctx, holders...)
	network := "canyon-app-" + app
	n, exists := e.network(ctx, network) // read after the stop and the holders: what is still on it is the joined containers and the door
	if exists {
		for _, c := range n.members() {
			if out, err := child(ctx, e.docker(), "network", "disconnect", network, c).CombinedOutput(); err != nil {
				log.Printf("executor: network disconnect %s %s: %s", network, c, strings.TrimSpace(string(out)))
				return fmt.Errorf("network disconnect %s %s: %w", network, c, err)
			}
		}
		if out, err := child(ctx, e.docker(), "network", "rm", network).CombinedOutput(); err != nil {
			log.Printf("executor: network rm %s: %s", network, strings.TrimSpace(string(out)))
			return fmt.Errorf("network rm %s: %w", network, err)
		}
	}
	if _, err := e.createNetwork(ctx, network, internal, n.subnet()); err != nil {
		return err
	}
	_, err = e.ensureNetwork(ctx, network, internal)
	return err
}

// MaxCallers bounds the addresses one POST /callers asks about.
const MaxCallers = 1024

// Callers answers who holds each address now (contracts/executor.md, POST /callers): an address held by
// exactly one container on a canyon-app-<app> network — <app>-<target> or its -previous, by name — at
// one of that target's fixed addresses there. Any other is unknown and left out: one docker handed out,
// one claimed twice, one no container holds, one whose State.StartedAt docker does not say. In the order asked;
// run is the label canyon.gate_run, since the container's State.StartedAt. Read only; a daemon that does not
// answer is an error.
func (e *Executor) Callers(ctx context.Context, addresses []string) ([]Caller, error) {
	asked, err := CallerAddresses(addresses)
	if err != nil {
		return nil, err
	}
	out, err := child(ctx, e.docker(), "network", "ls", "--filter", "name=canyon-app-", "--format", "{{.Name}}").Output()
	if err != nil {
		return nil, fmt.Errorf("docker network ls: %w", err)
	}
	names := slices.DeleteFunc(strings.Fields(string(out)), func(n string) bool { return !strings.HasPrefix(n, "canyon-app-") }) // the filter matches anywhere in a name
	if len(names) == 0 {
		return []Caller{}, nil
	}
	out, err = child(ctx, e.docker(), append([]string{"network", "inspect"}, names...)...).Output()
	var nets []appNet
	if err == nil {
		err = json.Unmarshal(out, &nets)
	}
	if err != nil {
		return nil, fmt.Errorf("docker network inspect: %w", err)
	}
	claims := map[netip.Addr]int{}
	found := map[netip.Addr]Caller{}
	ids := map[netip.Addr]string{}
	for _, n := range nets {
		app := strings.TrimPrefix(n.Name, "canyon-app-")
		for id, c := range n.Containers {
			p, err := netip.ParsePrefix(c.IPv4Address)
			a := p.Addr()
			if err != nil || !asked[a] {
				continue
			}
			claims[a]++
			for target, hosts := range slots {
				if n.Internal && validApp(app) && (c.Name == app+"-"+target || c.Name == app+"-"+target+"-previous") && (a == at(n.subnet(), hosts[0]) || a == at(n.subnet(), hosts[1])) {
					found[a], ids[a] = Caller{Address: a.String(), App: app, Target: target, Kind: "web"}, id
				}
			}
		}
	}
	for a := range found {
		if claims[a] != 1 {
			delete(found, a)
			delete(ids, a)
		}
	}
	held, err := e.callerHolders(ctx, ids)
	if err != nil {
		return nil, err
	}
	answer := []Caller{}
	for _, s := range addresses {
		a := netip.MustParseAddr(s)
		if c, ok := found[a]; ok && !held[ids[a]].since.IsZero() { // a holder whose start is not read is unknown
			c.Run, c.Since = held[ids[a]].run, held[ids[a]].since
			answer = append(answer, c)
		}
	}
	return answer, nil
}

// CallerAddresses is a POST /callers body's addresses as a set: 1 to MaxCallers distinct IPv4 addresses, each as
// netip writes it, else ErrRequest.
func CallerAddresses(addresses []string) (map[netip.Addr]bool, error) {
	if len(addresses) == 0 || len(addresses) > MaxCallers {
		return nil, fmt.Errorf("%w: ask about 1 to %d addresses", ErrRequest, MaxCallers)
	}
	asked := map[netip.Addr]bool{}
	for _, s := range addresses {
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Is4() || a.String() != s || asked[a] {
			return nil, fmt.Errorf("%w: addresses are distinct IPv4 addresses: %q", ErrRequest, s)
		}
		asked[a] = true
	}
	return asked, nil
}

// ValidRun holds when run is the server's run id as /run's run and the label canyon.gate_run carry it.
func ValidRun(run string) bool { return runRe.MatchString(run) }

// holder is a container POST /callers names: its label canyon.gate_run, nil when it has none that is a run
// id, and when it last started, in UTC — after docker gave it its address on start, never its Created, which
// is before — zero when docker did not say or it never started.
type holder struct {
	run   *string
	since time.Time
}

// callerHolders is each container named, by id, from one docker container inspect; one the inspect leaves out is
// absent.
func (e *Executor) callerHolders(ctx context.Context, ids map[netip.Addr]string) (map[string]holder, error) {
	held := map[string]holder{}
	if len(ids) == 0 {
		return held, nil
	}
	args := []string{"container", "inspect"}
	for _, id := range ids {
		args = append(args, id)
	}
	out, err := child(ctx, e.docker(), args...).Output()
	var inspected []struct {
		ID    string `json:"Id"`
		State struct {
			StartedAt time.Time `json:"StartedAt"`
		} `json:"State"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err == nil {
		err = json.Unmarshal(out, &inspected)
	}
	if err != nil {
		return nil, fmt.Errorf("docker container inspect: %w", err)
	}
	for _, c := range inspected {
		h := holder{since: c.State.StartedAt.UTC()}
		if run := c.Config.Labels["canyon.gate_run"]; runRe.MatchString(run) {
			h.run = &run
		}
		held[c.ID] = h
	}
	return held, nil
}
