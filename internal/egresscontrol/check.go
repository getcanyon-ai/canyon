package egresscontrol

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/envoyproxy/go-control-plane/pkg/server/sotw/v3"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/getcanyon-ai/canyon/internal/destination"
)

// decision is one call's answer (contracts/routes.md, The check, Interception): its caller, the destination as a
// record keeps it, and the reason it is refused, "" when it is allowed. fresh is whether the name took one of the
// minute's new names. On an OK: set, the headers it overwrites, each a name and a value; remove, the headers it
// removes; and intercept, whether it sends a CONNECT to the internal listener, naming its caller in the metadata.
type decision struct {
	app, env    string
	ae          appEnv
	destination string
	public      string
	reason      string
	fresh       bool
	name        string // the destination canonical, once Canonical took it
	port        int
	route       *route // the caller's route to name, nil for none
	intercept   bool
	injected    bool // an absolute-form OK that set the route's header: its metadata names the destination
	limit       int  // a rate_limited refusal's cap: the new names the app env may call a minute, across every replica
	set         [][2]string
	remove      []string
}

// Check answers Envoy. An outer check (ext_authz #1) reads the direct peer's address, whether the method is CONNECT,
// the scheme, the authority and Proxy-Authorization, a pod's token on Kubernetes; an inner one (ext_authz #2, its context naming the destination of the internal
// listener's chain) reads x-canyon-caller, x-canyon-destination, that destination and the authority. Nothing else of the request is read,
// and none of it logged: an absolute-form call's path and query are in it.
func (c *Control) Check(_ context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	deadline := time.Now().Add(c.budget)
	attrs := req.GetAttributes()
	http := attrs.GetRequest().GetHttp()
	var d decision
	if dest, inner := attrs.GetContextExtensions()["destination"]; inner {
		d = c.inner(deadline, dest, http.GetHeaders()["x-canyon-caller"], http.GetHeaders()["x-canyon-destination"], http.GetHost())
	} else {
		connect := http.GetMethod() == "CONNECT"
		d = c.decide(attrs.GetSource().GetAddress().GetSocketAddress().GetAddress(), http.GetHeaders()["proxy-authorization"], connect, http.GetScheme(), http.GetHost())
		c.inject(&d, connect, http.GetScheme(), deadline)
		if d.app != "" && d.reason != "rate_limited" && d.reason != "invalid_destination" && d.reason != "port" {
			c.settle(d.ae, d.destination, d.fresh, d.reason == "")
		}
	}
	if d.reason != "" {
		c.mu.Lock()
		c.refusals[d.reason]++
		c.mu.Unlock()
		stream := http.GetId()
		if len(stream) > streamIDSize {
			stream = ""
		}
		c.log.Printf("egress-control: refused: app %q env %q destination %q reason %s stream %q", d.app, d.env, d.destination, d.reason, stream)
	}
	return d.response(), nil
}

// decide is The check's decision, in its order: unknown_caller; invalid_destination, or port when the port
// alone is refused; rate_limited; a platform destination, allowed; observe, allowed; allowlist, allowed when
// an entry allows the destination, else not_allowed; a mode that is neither, not_allowed. auth is the call's
// Proxy-Authorization, which an OK removes before the call moves on.
func (c *Control) decide(source, auth string, connect bool, scheme, authority string) decision {
	h, ae, ok := c.who(source, auth)
	port := 80
	if connect || scheme == "https" {
		port = 443
	}
	var d decision
	if auth != "" {
		d.remove = []string{"proxy-authorization"}
	}
	d.destination, _ = destination.Recorded(authority, port)
	if !ok {
		d.reason = "unknown_caller"
		return d
	}
	d.app, d.env, d.ae, d.public = ae.app, ae.env, ae, h.public
	name, port, err := destination.Canonical(authority, port)
	if err != nil {
		d.reason = "invalid_destination"
		if errors.Is(err, destination.ErrPort) {
			d.reason = "port"
		}
		return d
	}
	d.name, d.port = name, port
	var admitted bool
	if admitted, d.fresh = c.admit(h, ae, name); !admitted {
		d.reason, d.limit = "rate_limited", h.newNames
		return d
	}
	d.reason = h.refusal(ae, name)
	// on Kubernetes a caller who carries a token was named by it (who): one named by its pod's address alone is never
	// intercepted (§4.3)
	if r, routed := h.envs[ae].routes[name]; routed && (!c.kube || auth != "") {
		d.route = &r
	}
	return d
}

// who is the caller (design/EGRESS.md §4.3): on a Kubernetes host a call carrying a token is the env its valid token
// names, whatever its address and whether or not the env is token_only, and no one else, never its address's; and only
// while the server said the policy is current within callersKept, as an address's caller, and the key set within
// keysKept: a token caller is never trusted longer than an address caller. A call with none is its address's, but
// never a token_only env's (no downgrade). On Docker every caller is its address's, as before.
func (c *Control) who(source, auth string) (*held, appEnv, bool) {
	c.mu.Lock()
	h, current := c.held, c.current
	c.mu.Unlock()
	if h != nil && c.kube && auth != "" { // c.kube, never the policy's: a Kubernetes replica holds a policy carrying kubernetes alone (parse)
		// callersKept trips first while it is the shorter; keysKept holds the key set to its own bound should
		// callersKept ever grow past it
		if age := c.now().Sub(current); age > callersKept || age > keysKept {
			return h, appEnv{}, false
		}
		ae, ok := h.kube.caller(auth, c.now())
		return h, ae, ok
	}
	h, ae, ok := c.caller(source)
	if !ok {
		c.refetch(source)
		h, ae, ok = c.caller(source)
	}
	if ok && c.kube && h.envs[ae].tokenOnly {
		return h, appEnv{}, false
	}
	return h, ae, ok
}

// refusal is the decision past the caller, the canonicalisation and the rate limit, for both hops: ae's call to name
// refused for its reason, "" to let it through.
func (h *held) refusal(ae appEnv, name string) string {
	env := h.envs[ae]
	return Refusal(name, h.platform[name], env.mode, env.allowed)
}

// inject is what a route changes of a call the decision allowed (contracts/routes.md, Interception), decided by the
// policy's routes alone, never by the chains the listener serves: a CONNECT to 443, the OK that sends it to the
// internal listener; an absolute-form https:// request, no interception, the route's header overwritten on this
// answer, inject_unavailable when its value cannot be read in time; any other request to the destination — a plain
// http://, on 80 or naming 443, or a CONNECT to 80 — inject_requires_tls; and any request to it at all without Canyon
// Enterprise's Injector, inject_unavailable. Every other OK removes x-canyon-intercept.
func (c *Control) inject(d *decision, connect bool, scheme string, deadline time.Time) {
	switch {
	case d.reason != "":
		return
	case d.route == nil:
	case c.inj == nil: // without Canyon Enterprise nothing adds the credential: never sent without it
		d.reason = "inject_unavailable"
		return
	case d.port != 443 || (!connect && scheme != "https"):
		d.reason = "inject_requires_tls"
		return
	case connect:
		d.intercept, d.set = true, [][2]string{{"x-canyon-intercept", "1"}}
		return
	default:
		if !c.overwrite(d, *d.route, routeKey{d.ae, d.name}, deadline) {
			return
		}
		d.injected = true
	}
	d.remove = append(d.remove, "x-canyon-intercept")
}

// inner is ext_authz #2's decision on a request inside an intercepted tunnel, in its order: a caller empty — what an
// inner hop that skipped ext_authz #1 carries — or one the policy does not name, unknown_caller; a tunnel ext_authz #1
// approved for a destination other than dest (approved, empty for none), or an authority other than dest, compared
// canonically, misdirected; the decision ext_authz #1 makes, made again of dest — an allowlist entry removed or a mode
// changed since the tunnel opened — its reason; the caller's route to dest gone, or its value not read in time,
// inject_unavailable. Else the route's header is overwritten, whatever the app sent, and x-canyon-caller and
// x-canyon-destination removed. The caller and the approval are the headers' alone: Envoy overwrites both from the
// tunnel's filter state, and the inner request's source is Envoy's own, no app's.
func (c *Control) inner(deadline time.Time, dest, caller, approved, authority string) decision {
	d := decision{destination: dest}
	app, env, named := strings.Cut(caller, "/")
	c.mu.Lock()
	h := c.held
	fresh := h != nil && c.now().Sub(c.current) <= callersKept
	c.mu.Unlock()
	ae := appEnv{app, env}
	if _, known := h.envsOf()[ae]; !named || !fresh || !known {
		d.reason = "unknown_caller"
		return d
	}
	d.app, d.env, d.ae, d.public = app, env, ae, h.public
	if approved != dest {
		if name, _, err := destination.Canonical(approved, 443); err == nil && name == approved {
			d.destination = approved // the body names what the tunnel goes to
		}
		d.reason = "misdirected"
		return d
	}
	if canonicalAuthority(authority) != dest {
		d.reason = "misdirected"
		return d
	}
	if d.reason = h.refusal(ae, dest); d.reason != "" {
		return d
	}
	r, routed := h.envs[ae].routes[dest]
	if !routed {
		d.reason = "inject_unavailable"
		return d
	}
	if c.overwrite(&d, r, routeKey{ae, dest}, deadline) {
		d.remove = []string{"x-canyon-caller", "x-canyon-destination"}
	}
	return d
}

// envsOf is h's envs, none when h is nil.
func (h *held) envsOf() map[appEnv]envPolicy {
	if h == nil {
		return nil
	}
	return h.envs
}

// overwrite sets r's header on d from its value, else refuses d inject_unavailable, and answers whether it set it.
func (c *Control) overwrite(d *decision, r route, k routeKey, deadline time.Time) bool {
	if c.inj == nil {
		d.reason = "inject_unavailable"
		return false
	}
	v, err := c.inj.Value(deadline, k.ae.app, k.ae.env, k.destination, r.scheme, r.rev)
	if err != nil {
		d.reason = "inject_unavailable"
		return false
	}
	d.set = [][2]string{{r.header, v}}
	return true
}

// canonicalAuthority is an inner request's :authority or Host as the destination is compared with it: lower-case and
// a :443 dropped. Its one trailing dot is dropped before it comes here, by the chain's connection manager
// (strip_trailing_host_dot), which the chain's routes match after too, so a dot it leaves — the second of
// api.stripe.com.. — is another name to both, and the two never disagree. One with a byte past ASCII is none, as
// lower-casing it could fold a sign into a letter (the Kelvin sign into k).
func canonicalAuthority(a string) string {
	if strings.ContainsFunc(a, func(r rune) bool { return r >= 0x80 }) {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(a), ":443")
}

// Refusal is the check's answer on a name past Canonical and the rate limit, "" to let it through: a platform
// destination in any mode, observe, and allowlist when an entry allows it, else not_allowed, a mode that is neither
// among them. The go-live rule asks the same of production, so the two never differ.
func Refusal(name string, platform bool, mode string, allowed []string) string {
	switch {
	case platform, mode == "observe":
		return ""
	case mode == "allowlist" && slices.ContainsFunc(allowed, func(entry string) bool { return destination.Allows(entry, name) }):
		return ""
	}
	return "not_allowed"
}

func (d decision) body() string {
	switch d.reason {
	case "not_allowed":
		return "Canyon refused this call: " + d.destination + " is not on " + d.app + " " + d.env + "'s outbound allowlist. A person on the app's team can allow it in Canyon: " + d.public + "/#/apps/" + d.app + "/outbound"
	case "unknown_caller":
		return "Canyon refused this call: Canyon does not know which app is calling."
	case "invalid_destination":
		return "Canyon refused this call: an app may call a host name on port 80 or 443, never an IP address."
	case "port":
		return "Canyon refused this call: an app may call out on port 80 or 443 only."
	case "inject_requires_tls":
		return "Canyon refused this call: use https:// for " + d.destination + ". Canyon adds the app's credential only over https."
	case "inject_unavailable":
		return "Canyon could not add the app's credential for " + d.destination + ", so it did not send this call. Try again. If this persists, ask an admin."
	case "misdirected":
		return "Canyon refused this call: this connection goes to " + d.destination + ", but the request is for another host."
	default: // rate_limited
		return "Canyon refused this call: " + d.app + " " + d.env + " called more than " + strconv.Itoa(d.limit) + " new destinations this minute. It can call them from the next minute, or an admin can raise the limit in Settings › Outbound."
	}
}

// status is a refusal's: 421 misdirected, 502 inject_unavailable, else 403.
func (d decision) status() typev3.StatusCode {
	switch d.reason {
	case "misdirected":
		return typev3.StatusCode_MisdirectedRequest
	case "inject_unavailable":
		return typev3.StatusCode_BadGateway
	}
	return typev3.StatusCode_Forbidden
}

// response is OK, with the headers it sets and removes, or the refusal's status with the reason and its line, and on
// either the dynamic metadata the call's access-log entry carries to the receiver: the caller's app and env, outcome
// and the reason; and on an OK that intercepts, the caller as <app>/<env> and the destination it approved, canonical,
// which Envoy copies to canyon.caller and canyon.destination.
func (d decision) response() *authv3.CheckResponse {
	md := map[string]any{"outcome": "allowed"}
	if d.reason != "" {
		md["outcome"] = "refused"
	}
	if d.app != "" {
		md["app"], md["env"] = d.app, d.env
	}
	if d.reason != "" {
		md["reason"] = d.reason
	}
	if d.intercept {
		md["caller"], md["destination"] = d.app+"/"+d.env, d.name
	}
	if d.injected { // Envoy's local reply tells a destination with a route by it (contracts/routes.md, Interception)
		md["destination"] = d.name
	}
	meta, _ := structpb.NewStruct(md) // strings alone: never an error
	header := func(k, v string) *corev3.HeaderValueOption {
		return &corev3.HeaderValueOption{Header: &corev3.HeaderValue{Key: k, Value: v}, AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD}
	}
	if d.reason == "" {
		ok := &authv3.OkHttpResponse{HeadersToRemove: d.remove}
		for _, kv := range d.set {
			ok.Headers = append(ok.Headers, header(kv[0], kv[1]))
		}
		return &authv3.CheckResponse{
			Status:          &rpcstatus.Status{Code: int32(codes.OK)},
			HttpResponse:    &authv3.CheckResponse_OkResponse{OkResponse: ok},
			DynamicMetadata: meta,
		}
	}
	return &authv3.CheckResponse{
		Status: &rpcstatus.Status{Code: int32(codes.PermissionDenied)},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: &authv3.DeniedHttpResponse{
			Status:  &typev3.HttpStatus{Code: d.status()},
			Headers: []*corev3.HeaderValueOption{header("Canyon-Egress", d.reason), header("content-type", "text/plain; charset=utf-8")},
			Body:    d.body(),
		}},
		DynamicMetadata: meta,
	}
}

// Serve answers Envoy's checks on ln, a loopback address beside it, until ctx ends, its aggregated discovery
// stream (LDS and SDS: xds.go), and the gRPC health check Probe asks: SERVING once the replica holds a policy.
func Serve(ctx context.Context, ln net.Listener, c *Control) error {
	srv := grpc.NewServer()
	authv3.RegisterAuthorizationServer(srv, c)
	discoveryv3.RegisterAggregatedDiscoveryServiceServer(srv, ads{sotw: sotw.NewServer(ctx, c.snapshots, refusals{c.log})})
	healthpb.RegisterHealthServer(srv, readiness{c: c})
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	return srv.Serve(ln)
}

// OpenEntryTag is the custom tag every access-log entry of Canyon's bootstrap carries (deploy/egress/envoy.yaml): this
// Envoy logs a tunnel as it opens, so the tunnel's last entry does not count its call (contracts/routes.md).
const OpenEntryTag = "canyon.tunnel_open_entry"

// readiness answers gRPC's health check from Ready.
type readiness struct {
	healthpb.UnimplementedHealthServer
	c *Control
}

func (r readiness) Check(context.Context, *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	if r.c.Ready() {
		return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
	}
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_NOT_SERVING}, nil
}

// Probe asks the replica on addr, a loopback address, whether it holds a policy: nil only if it does, so an exec
// probe (canyon-server egress-control probe) can tell readiness, which no probe of the pod's address reaches.
func Probe(ctx context.Context, addr string) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		return err
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return errors.New("egress-control holds no policy yet")
	}
	return nil
}
