package executor

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"
)

// appFloor is an app holder's entrypoint: the floor, then sleep (contracts/executor.md, An app reaches no
// private, link-local or host address).
//
//go:embed appfloor.sh
var appFloor string

// holderLabel marks an app's floor holder, and floorLabel names, on the app's container, the holder whose
// network namespace it runs in.
const (
	holderLabel = "canyon.holder"
	floorLabel  = "canyon.floor"
)

// holderRe is a holder's name, which the executor alone gives: canyon.holder and canyon.floor are labels, which an
// app's image may carry too, so a container is an app's holder only under this name as well.
var holderRe = regexp.MustCompile(`^canyon-appfloor-[0-9a-f]{8}$`)

// holdFloor starts the holder an app's container on the open network n runs in, canyon-appfloor-<nonce>, and
// answers its name once the floor is in place in its namespace: the holder carries the app's network
// endpoint and its name, <app>-<target>, as an alias, so the server reaches the app as before. Its floor refuses
// host, the Docker host's own addresses, which the caller read before taking e.nets. Anything short of that is an
// error starting "app network: ", and no holder is left. The caller holds e.nets, so a reap never finds the holder
// before the app that names it.
func (e *Executor) holdFloor(ctx context.Context, n appNet, app, target string, host []netip.Prefix) (string, error) {
	sub, gw := n.subnet(), n.gateway()
	if !sub.IsValid() || !sub.Contains(gw) {
		return "", fmt.Errorf("app network: %s is not a /24 with its gateway in it: nothing to floor an app by", n.Name)
	}
	if len(host) == 0 {
		return "", fmt.Errorf("app network: the addresses of the Docker host: none read")
	}
	name := "canyon-appfloor-" + nonce()
	args := append([]string{"run", "-d", "--name", name, "--network", n.Name, "--network-alias", app + "-" + target, "--restart", "no",
		"--label", "canyon.app=" + app, "--label", "canyon.target=" + target, "--label", holderLabel}, e.projectLabel()...)
	args = append(args, "--cap-drop", "ALL", "--cap-add", "NET_ADMIN", "--security-opt", "no-new-privileges", "--read-only",
		"--memory", "32m", "--pids-limit", "32", "--entrypoint", "/bin/sh",
		"--env", "CANYON_APP_GATEWAY="+gw.String(), "--env", "CANYON_APP_SUBNET="+sub.String(), "--env", "CANYON_APP_HOST="+prefixes(host),
		"--env", "CANYON_APP_POOLS="+prefixes(e.Refused()), "--env", "CANYON_APPS_ALLOW="+strings.Join(e.AppsAllow, ","),
		e.holder(), "-c", appFloor)
	out, err := child(ctx, e.docker(), args...).CombinedOutput()
	if err != nil {
		err = fmt.Errorf("the holder %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	} else {
		err = e.floorIn(ctx, name, "CANYON-APP")
	}
	if err != nil {
		e.removeHolders(ctx, name) // a run the daemon refused may have created the container all the same
		return "", fmt.Errorf("app network: %w", err)
	}
	return name, nil
}

// hostAddrs is every IPv4 address of the Docker host's own, each a /32, as `ip addr show` lists them from the host's
// network namespace — on Docker Desktop and Colima the VM's — read by the port check's busybox, which joins that
// namespace and nothing else, as user 65534 with no capability. A host with no address but loopback, or a listing
// that fails, is an error: a floor that does not know the host's addresses would let an app reach a public one.
func (e *Executor) hostAddrs(ctx context.Context) ([]netip.Prefix, error) {
	out, err := child(ctx, e.docker(), "run", "--rm", "--network", "host", "--user", "65534:65534", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", cmp.Or(e.Busybox, busyboxImage), "ip", "addr", "show").Output()
	if err != nil {
		return nil, fmt.Errorf("the addresses of the Docker host: %w", err)
	}
	var addrs []netip.Prefix
	routable := false
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "inet" { // "inet <address>/<bits> …": an IPv4 address of an interface's
			continue
		}
		p, err := netip.ParsePrefix(f[1])
		if err != nil || !p.Addr().Is4() {
			return nil, fmt.Errorf("the addresses of the Docker host: %q is no IPv4 address", f[1])
		}
		if a := netip.PrefixFrom(p.Addr(), 32); !slices.Contains(addrs, a) {
			addrs = append(addrs, a)
		}
		routable = routable || !p.Addr().IsLoopback()
	}
	if !routable {
		return nil, fmt.Errorf("the addresses of the Docker host: none but loopback listed")
	}
	return addrs, nil
}

// removeHolders removes the holders named, even past ctx's end. It logs; it never fails.
func (e *Executor) removeHolders(ctx context.Context, names ...string) {
	if len(names) == 0 {
		return
	}
	if out, err := child(context.WithoutCancel(ctx), e.docker(), append([]string{"rm", "-f"}, names...)...).CombinedOutput(); err != nil {
		log.Printf("executor: rm %v: %s", names, strings.TrimSpace(string(out)))
	}
}

// holders is every floor holder of app's, running or not, by name: labelled a holder and the app's, and named as
// the executor names one, so a container whose image carries the labels is never one. A daemon that cannot list
// is an error.
func (e *Executor) holders(ctx context.Context, app string) ([]string, error) {
	out, err := child(ctx, e.docker(), "container", "ls", "--all", "--filter", "label="+holderLabel, "--filter", "label=canyon.app="+app, "--format", "{{.Names}}").Output()
	if err != nil {
		return nil, fmt.Errorf("docker container ls: %w", err)
	}
	return slices.DeleteFunc(strings.Fields(string(out)), func(n string) bool { return !holderRe.MatchString(n) }), nil
}

// reap removes every holder of app's that none of its containers — <app>-<target> and the -previous a
// redeploy sets aside — names in canyon.floor: what a redeploy, a failed start or a /stop left. A daemon
// that cannot answer leaves every holder: a holder an app still runs in is never removed.
func (e *Executor) reap(ctx context.Context, app string) {
	e.nets.Lock()
	defer e.nets.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), netDeadline)
	defer cancel()
	if err := e.reapLocked(ctx, app); err != nil {
		log.Printf("executor: holders of %s: %v; none removed", app, err)
	}
}

// reapLocked is reap for a caller that holds e.nets.
func (e *Executor) reapLocked(ctx context.Context, app string) error {
	holders, err := e.holders(ctx, app)
	if err != nil || len(holders) == 0 {
		return err
	}
	named := map[string]bool{}
	listed, err := e.containers(ctx, app+"-preview", app+"-prod", app+"-preview-previous", app+"-prod-previous")
	if err != nil {
		return err
	}
	if len(listed) > 0 {
		args := []string{"container", "inspect", "--format", `{{index .Config.Labels "` + floorLabel + `"}}`}
		for name := range listed {
			args = append(args, name)
		}
		out, err := child(ctx, e.docker(), args...).Output()
		if err != nil {
			return fmt.Errorf("docker container inspect: %w", err)
		}
		for _, h := range strings.Fields(string(out)) {
			named[h] = true
		}
	}
	e.removeHolders(ctx, slices.DeleteFunc(holders, func(h string) bool { return named[h] })...)
	return nil
}

// floorState is what docker container inspect says of an app's container or its holder.
type floorState struct {
	Name       string `json:"Name"` // with docker's leading slash
	ID         string `json:"Id"`
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"` // container:<the ID of the container whose namespace it joins>, as docker resolves it
	} `json:"HostConfig"`
	State struct {
		Running   bool      `json:"Running"`
		StartedAt time.Time `json:"StartedAt"`
		Error     string    `json:"Error"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// Reconcile keeps every app that runs in a holder floored and running, in order (contracts/executor.md, An
// app reaches no private, link-local or host address): docker restarts neither a holder (--restart no) nor,
// while its holder is down, the app, which cannot join a namespace that is not there. One pass lists the
// containers labelled canyon.app once and inspects them once (and, once, any holder they name outside the
// list), without e.nets; it takes e.nets only to act, and inspects the pair again under it first. For each
// <app>-<target> whose holder exists and is proved its own (owned), what step says is done. The same pass
// reaps, under e.nets, each holder no container of its app names, and logs each app on an open network that
// runs in no holder: one an executor from before the floor started, which only its next deploy floors.
func (e *Executor) Reconcile(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, netDeadline)
	defer cancel()
	out, err := child(ctx, e.docker(), "container", "ls", "--all", "--filter", "label=canyon.app", "--format", "{{.Names}}").Output()
	if err != nil {
		log.Printf("executor: reconcile: docker container ls: %v", err)
		return
	}
	names := strings.Fields(string(out))
	if len(names) == 0 {
		return
	}
	seen, err := e.inspectAll(ctx, names...)
	if err != nil {
		log.Printf("executor: reconcile: %v", err) // one removed since the listing; the rest are still read
	}
	var outside []string // holders named outside the listing: none the executor made, so each is refused below
	for _, c := range seen {
		if h := c.Config.Labels[floorLabel]; h != "" && !slices.Contains(names, h) && !slices.Contains(outside, h) {
			outside = append(outside, h)
		}
	}
	if len(outside) > 0 {
		more, _ := e.inspectAll(ctx, outside...) // one gone is no holder to act on
		maps.Copy(seen, more)
	}
	named := map[string]bool{}
	for _, c := range seen {
		if h := c.Config.Labels[floorLabel]; h != "" {
			named[h] = true
		}
	}
	orphaned := map[string]bool{}
	unfloored := map[string]string{} // container → its network
	slices.Sort(names)
	for _, name := range names {
		c, there := seen[name]
		if !there {
			continue
		}
		if _, holds := c.Config.Labels[holderLabel]; holds && holderRe.MatchString(name) {
			if !named[name] {
				orphaned[c.Config.Labels["canyon.app"]] = true
			}
			continue
		}
		app, target, ok := appTarget(name)
		if !ok { // a -previous belongs to a /run under way, or to the next one
			continue
		}
		holder := c.Config.Labels[floorLabel]
		if holder == "" {
			if c.HostConfig.NetworkMode == "canyon-app-"+app {
				unfloored[name] = c.HostConfig.NetworkMode
			}
			continue
		}
		h, there := seen[holder]
		if !there { // gone, as POST /networks/recreate leaves it: the app runs again at its next /run alone, never unfloored
			continue
		}
		if err := owned(name, c, holder, h, app, target); err != nil {
			log.Printf("executor: reconcile %s: %v", name, err)
			continue
		}
		if step(c, h) == stay {
			continue
		}
		if err := e.reconcileLocked(ctx, name, app, target); err != nil {
			log.Printf("executor: reconcile %s: %v", name, err)
		}
	}
	for app := range orphaned {
		if validApp(app) {
			e.reap(ctx, app)
		}
	}
	e.logUnfloored(ctx, unfloored)
}

// appTarget splits <app>-<target>; a -previous, or any other name, is not one.
func appTarget(name string) (app, target string, ok bool) {
	for _, target := range []string{"preview", "prod"} {
		if app, ok := strings.CutSuffix(name, "-"+target); ok && validApp(app) {
			return app, target, true
		}
	}
	return "", "", false
}

// owned is nil when holder, which the app's container c names in canyon.floor, is the app's own: canyon.floor is a
// label, which an image may carry too, so the holder must be named and labelled as holdFloor makes one, and be the
// very container whose namespace docker joined the app to, which no image sets.
func owned(name string, c floorState, holder string, h floorState, app, target string) error {
	if _, labelled := h.Config.Labels[holderLabel]; !holderRe.MatchString(holder) || h.Name != "/"+holder || !labelled ||
		h.Config.Labels["canyon.app"] != app || h.Config.Labels["canyon.target"] != target || h.ID == "" || c.HostConfig.NetworkMode != "container:"+h.ID {
		return fmt.Errorf("refused: %s names %s in %s, which is not the floor holder it runs in; nothing started or stopped", name, holder, floorLabel)
	}
	return nil
}

// what a reconcile does with an app and its own holder.
type reconcileStep int

const (
	stay       reconcileStep = iota // in order, or stopped by itself: as docker leaves it
	moveApp                         // the holder down under the running app: the app stopped, the holder started, then the app
	startBoth                       // the holder down, the app's start refused for want of it: the holder started, then the app
	startApp                        // the holder running, the app's last start failed: the app started on the floor
	restartApp                      // the holder started after the app: the app stopped, then started in its namespace
)

func step(c, h floorState) reconcileStep {
	switch {
	case !h.State.Running && c.State.Running:
		return moveApp
	case !h.State.Running && c.State.Error != "":
		return startBoth
	case h.State.Running && !c.State.Running && c.State.Error != "":
		return startApp
	case h.State.Running && c.State.Running && c.State.StartedAt.Before(h.State.StartedAt):
		return restartApp
	}
	return stay
}

// reconcileLocked acts on name under e.nets, its pair inspected again first: the app stopped (a no-op on a stopped
// one, which cancels any restart docker has pending), the holder started where it is not running, the floor
// waited for, then the app started. Once the app is stopped, any failure after leaves the holder stopped and asks
// docker to start the app, which docker refuses for want of the holder, recording State.Error: the next pass reads
// a refused start and starts both, where an app stopped with no error would stay down for good. It runs on a
// deadline of its own, never on what the pass's listing and inspects left (#2523), and a final start that fails
// goes the way of any failure after the app's stop.
func (e *Executor) reconcileLocked(ctx context.Context, name, app, target string) error {
	e.nets.Lock()
	defer e.nets.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), netDeadline)
	defer cancel()
	seen, _ := e.inspectAll(ctx, name)
	c, there := seen[name]
	holder := c.Config.Labels[floorLabel]
	if !there || holder == "" {
		return nil
	}
	pair, _ := e.inspectAll(ctx, holder)
	h, there := pair[holder]
	if !there {
		return nil
	}
	if err := owned(name, c, holder, h, app, target); err != nil {
		return err
	}
	s := step(c, h)
	if s == stay {
		return nil
	}
	do := func(ctx context.Context, verb, of string) error {
		if out, err := child(ctx, e.docker(), verb, "--", of).CombinedOutput(); err != nil { // a name read from a label is never a flag
			return fmt.Errorf("docker %s %s: %w: %s", verb, of, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := do(ctx, "stop", name); err != nil {
		return err
	}
	failed := func(err error) error { // the app stopped: the holder left stopped, the app's start left refused, its error recorded
		keep, cancel := context.WithTimeout(context.WithoutCancel(ctx), netDeadline) // its own time, whatever the step before used
		defer cancel()
		if serr := do(keep, "stop", holder); serr != nil { // fail closed: never started beside a holder that may run unfloored
			return fmt.Errorf("%w; %v, so %s stays stopped", err, serr, name)
		}
		after, ierr := e.inspectAll(keep, holder)
		if st, there := after[holder]; ierr != nil || !there || st.State.Running {
			return fmt.Errorf("%w; %s is not known stopped (%v), so %s stays stopped", err, holder, ierr, name)
		}
		_ = do(keep, "start", name) // refused for want of the holder: what the next pass reads
		return err
	}
	if s == moveApp || s == startBoth {
		if err := do(ctx, "start", holder); err != nil {
			return failed(err)
		}
	}
	if err := e.floorIn(ctx, holder, "CANYON-APP"); err != nil {
		return failed(err)
	}
	if err := do(ctx, "start", name); err != nil {
		return failed(err)
	}
	log.Printf("executor: reconcile: %s floored again in %s", name, holder)
	return nil
}

// logUnfloored logs each container of unfloored whose network is open: an app an executor from before the floor
// started, which runs on it directly until its next deploy. One inspect of their networks; one that cannot be read
// is logged as open, never passed over.
func (e *Executor) logUnfloored(ctx context.Context, unfloored map[string]string) {
	if len(unfloored) == 0 {
		return
	}
	var networks []string
	for _, n := range unfloored {
		if !slices.Contains(networks, n) {
			networks = append(networks, n)
		}
	}
	out, _ := child(ctx, e.docker(), append([]string{"network", "inspect"}, networks...)...).Output()
	var nets []appNet
	_ = json.Unmarshal(out, &nets)
	internal := map[string]bool{}
	for _, n := range nets {
		internal[n.Name] = n.Internal
	}
	for _, name := range slices.Sorted(maps.Keys(unfloored)) {
		if n := unfloored[name]; !internal[n] {
			if _, said := e.warned.LoadOrStore(name+" "+n, true); !said { // once a process, never every 15 s
				log.Printf("executor: %s runs on %s with no floor, deployed before the floor: redeploy it", name, n)
			}
		}
	}
}

// unfloored says whether name exists and runs in no holder's namespace: an app an executor from before the floor
// started. A daemon that does not answer is read as unfloored, so such a container is never named back.
func (e *Executor) unfloored(ctx context.Context, name string) bool {
	listed, err := e.containers(ctx, name)
	if err != nil {
		return true
	}
	if _, there := listed[name]; !there {
		return false
	}
	out, err := child(ctx, e.docker(), "container", "inspect", "--format", "{{.HostConfig.NetworkMode}}", name).Output()
	return err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "container:")
}

// inspectAll is docker container inspect's answer for names, in one call, by name. docker prints those that exist
// and fails for the rest: what it printed is answered with the error.
func (e *Executor) inspectAll(ctx context.Context, names ...string) (map[string]floorState, error) {
	out, err := child(ctx, e.docker(), append([]string{"container", "inspect", "--"}, names...)...).Output() // a name read from an image's label is never a flag
	var got []floorState
	if jerr := json.Unmarshal(out, &got); jerr != nil && err == nil {
		err = jerr
	}
	seen := map[string]floorState{}
	for _, c := range got {
		seen[strings.TrimPrefix(c.Name, "/")] = c
	}
	if err != nil {
		err = fmt.Errorf("docker container inspect: %w", err)
	}
	return seen, err
}

// gateway is the network's IPv4 gateway as docker reports it: the host's own address on it.
func (n appNet) gateway() netip.Addr {
	for _, c := range n.IPAM.Config {
		if a, err := netip.ParseAddr(c.Gateway); err == nil && a.Is4() {
			return a
		}
	}
	return netip.Addr{}
}
