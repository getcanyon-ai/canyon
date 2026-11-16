# Canyon on Kubernetes — Gateway API routes (mode B)

**Status: designed 2026-09-26, not built. Build on the owner's go (tracker: canyonv2-ula5.58).**

**The rule this document exists to enforce: an app's front door on Kubernetes is one named choice,
never an assumption.** `front_door` already says who signs people in (`choice`). This document adds
the other half — who routes the request to the pod at all — as one more explicit field, not a
side effect of what happens to be installed on the cluster.

---

## 1. Why B exists

An app's address on Kubernetes reaches its pod through exactly one of three doors:

- **A — Canyon's router.** Built. One wildcard rule of the org's own load balancer or ingress
  points at `canyon-router`, which proxies every app by its Host header
  (`deploy/k8s/router.yaml`, `contracts/executor.md`, *On Kubernetes*).
- **B — the org's own Gateway.** This document. The org already runs a Gateway API
  implementation; Canyon becomes an app owner on it, attaching one route per app environment.
- **C — hand-off.** Canyon builds, scans and gates; it pushes a digest-pinned image to the org's
  own registry, and the org's own pipeline deploys and routes every environment, previews
  included. Designed separately, `design/HAND-OFF.md`.

Per-app Ingress is not offered: ingress-nginx, the Ingress controller most self-hosted clusters
ran, was retired in March 2026, and every controller left routes TLS, class and auth through its
own vendor annotations — one dialect per controller, and this repo does not build a table for
that. Gateway API is the one-spec replacement.

**The standing fact under every mode.** The router's own NetworkPolicy admits its 8080 with no
`from:` at all (`deploy/k8s/router.yaml:96-97`) — a peer only bounds which pods may connect, never
whether the request inside is genuine. Trust that a request came through the AWS ALB (choice 1's
`alb`) has never rested on the network: it rests on `cmd/canyon-identity/alb.go`'s signature check
— `signer == alb_arn` (`alb.go:69`), unexpired (`alb.go:71`), `ecdsa.Verify` (`alb.go:81`) —
verified against `origin/main`. Mode B changes which pods may reach 4180. Nothing else.

## 2. The admin's side, done once

The org runs its own Gateway (Envoy Gateway, Istio, the AWS Load Balancer Controller, NGINX
Gateway Fabric, Kong, Traefik, kgateway, …) — one `Gateway` object with a listener whose hostname
covers the app addresses. It hands Canyon that object's namespace and name.

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: org-gateway, namespace: org-ingress}
spec:
  gatewayClassName: envoy-gateway    # whatever GatewayClass the org's implementation registers
  listeners:
    - name: canyon-apps-preview
      hostname: "*.preview.canyon.meridian.example"
      port: 443
      protocol: HTTPS
      tls: {certificateRefs: [{name: canyon-preview-tls}]}
      allowedRoutes:
        namespaces: {from: Selector, selector: {matchLabels: {kubernetes.io/metadata.name: canyon-apps}}}
    - name: canyon-apps-prod
      hostname: "*.prod.canyon.meridian.example"
      port: 443
      protocol: HTTPS
      tls: {certificateRefs: [{name: canyon-prod-tls}]}
      allowedRoutes:
        namespaces: {from: Selector, selector: {matchLabels: {kubernetes.io/metadata.name: canyon-apps}}}
```

Two listeners, for the certificate's sake, not the routing's. A wildcard certificate covers one
level: `*.preview.canyon.meridian.example`'s certificate matches
`claims-portal.preview.canyon.meridian.example` and nothing deeper — the probe already checks both
names separately for exactly this reason (`executor/kube/probe.go:272`, `certificate`). The
listener's own hostname match carries no such limit — Gateway API's wildcard matches any depth
(`*.example.com` matches `b.a.example.com`), so one listener could route both environments — the
split above exists only because each environment needs its own certificate. Under
`app-env.domain`, one certificate and one listener are enough.

`allowedRoutes` is what makes Canyon an app owner rather than the Gateway's operator: it admits
routes from `canyon-apps` and nothing else Canyon writes is visible here. DNS for the wildcard
name(s) points at the Gateway's own address — an AWS ALB under the AWS Load Balancer Controller's
`gateway.k8s.aws/alb` class (`HTTPRoute` needs a Layer-7 listener; the controller's NLB class is
Layer 4), an Istio ingress gateway Service, or whatever else fronts the implementation. None of
that is Canyon's to configure, the same way the router's DNS is the admin's today.

## 3. The front-door API change

Verified against `origin/main`, `contracts/routes.md:279`: `front_door` today is `{choice,
gateway?, issuer?, audience?, jwks_url?, cookie?, alb_arn?, signin?, domain, form, set_by,
set_at}`. `choice` already picks who signs people in (`1`: the org's OIDC gateway or an AWS ALB,
`2`: Canyon's own oauth2-proxy sidecar), and its own `gateway` key is already taken — a string,
`"oidc"` or `"alb"`, naming choice 1's kind (`contracts/executor.md:395-396`). B's Gateway object
reference nests under a new field instead, so it never collides with that key:

- **`route`**: `{via, gateway?, from?}`, nested like `signin` — absent means today's behaviour.
  - **`via`**: `"wildcard"` (default) or `"httproute"`.
  - **`gateway`**: `{namespace, name}` — the org's Gateway object; required when `via` is
    `httproute`. No `sectionName`: with the route's `hostnames` set, an `HTTPRoute` without one
    attaches to every listener on that Gateway whose hostname intersects the route's — exactly
    one, given how §2's listeners split by hostname — and is `Accepted: False /
    NoMatchingListenerHostname` on the Gateway that admits none, which §5's probe check catches
    before any app deploy would.
  - **`from`**: a list of NetworkPolicy peers, required (non-empty) when `via` is `httproute`.
    Each entry is either `{namespace, labels}` — the data-plane pods of a proxy that terminates
    the connection itself (Envoy, Istio's ingress gateway, NGINX Gateway Fabric, Traefik, Kong) —
    or `{cidr}` — a load balancer that opens the connection to the pod directly (an AWS ALB with
    `target-type: ip`, GCP NEGs).

Nesting means there is nothing to refuse when `route` is absent or `via` is `wildcard`: `gateway`
and `from` simply have nothing to apply to.

Refusals join the same order `contracts/routes.md:279` already documents, after `not_kubernetes`:

- `400 {error:"invalid_route"}` — `via` outside the two values.
- `400 {error:"invalid_front_door", message}` — the same catch-all the row already uses for a
  shape `/run`'s `front_door` refuses, now also covering: `via: "httproute"` without `gateway`, or
  with an empty `from`; a `from` entry that is neither `{namespace, labels}` nor `{cidr}` alone;
  `gateway.namespace` or `.name` not a valid Kubernetes name.
- `400 {error:"invalid_front_door", message:"an AWS load balancer sign-in is one listener rule: use
  route wildcard"}` — `route.via: "httproute"` together with choice 1's own `gateway: "alb"`. An
  ALB's OIDC auth action is one rule on one listener; it does not attach per app environment the
  way an `HTTPRoute` does. Choice 1 `oidc` and choice 2 both work under `httproute` (§7).

Under `run_by: org` (`design/HAND-OFF.md`), the route fields are not read, just as `domain` and
`form` are not.

The probe step (`POST /probe`) gains the check in §5; the same `409 probe_failed` /
`502 probe_unavailable` shape already wrapping it is unchanged.

## 4. Canyon's side, on each deploy of an app environment

Service and pod as today: the sidecar listens on 4180, the Service ships that port to that port.

The NetworkPolicy (`executor/kube/objects.go:68`, `networkPolicy`) keeps its shape and admits 4180
on ingress from `front_door.route.from` in place of the router's fixed peer (today: the `canyon`
namespace, pods labelled `canyon.front-door: true`) — nothing else changes: same egress, same pod
selector, same name.

One `HTTPRoute`, `canyon-apps/<app>-<target>`, joins the objects `write()` already creates in that
order — after the Service, before the Deployment (`executor/kube/run.go:39-44`: *"the
NetworkPolicy, the Service, the Secrets, the access ConfigMap, then the Deployment"*, so no route
exists before the policy that would admit its traffic) — and is deleted on `/stop`
(`executor/kube/run.go:307`, `remove`) alongside the Service and the NetworkPolicy.

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: claims-portal-preview
  namespace: canyon-apps
  labels: {canyon.app: claims-portal, canyon.target: preview}
spec:
  parentRefs:
    - {name: org-gateway, namespace: org-ingress}
  hostnames: ["claims-portal.preview.canyon.meridian.example"]
  rules:
    - backendRefs: [{name: claims-portal-preview, port: 4180}]
```

No `sectionName`: this route's `hostnames` intersects only the preview listener of §2's Gateway,
so it attaches there and nowhere else. Name and labels match every other object the executor
writes for this app (`executor/kube/objects.go:30-36`, `canyon.app`/`canyon.target`) — the same
`<app>-<target>` name the Service and NetworkPolicy already use, so all three are found, and
cleaned up, together.

Written with `sigs.k8s.io/gateway-api`'s typed clientset
(`sigs.k8s.io/gateway-api/pkg/client/clientset/versioned`, pinned at v1.6), not `unstructured` —
every other object here is typed against `k8s.io/api` (`go.mod:28`) and `k8s.io/client-go`
(`go.mod:30`).

## 5. The probe on save

`POST /probe` keeps what it answers today — `network_policy: enforced|not_enforced` and
`certificate: ok|missing`, unchanged (`executor/kube/probe.go:27-31`) — and, only when the body
being probed has `route.via: "httproute"`, adds one check: it writes a throwaway `HTTPRoute`,
`canyon-probe-preview`, hostname `canyon-probe.preview.<domain>`, parented on `route.gateway`, and
polls its status for up to 60 s waiting for `Accepted: True` — the same poll-then-sweep shape
`enforcement` already uses for the probe's Jobs (`executor/kube/probe.go:136-174`) — deleting it
regardless of the result.

`ProbeResult` gains one field:

```go
type ProbeResult struct {
    NetworkPolicy string `json:"network_policy"`
    Certificate   string `json:"certificate"`
    Route         string `json:"route,omitempty"` // ok | not_accepted — only under route.via: httproute
    Fix           string `json:"fix"`
}
```

`not_accepted`'s sentence joins `Fix` exactly as the network-policy and certificate fixes already
do (`probe.go:111-119`): *"The Gateway `<namespace>/<name>` did not accept a route from
`canyon-apps`. Allow routes from that namespace on its listener and save again."* — naming
`route.gateway`, the field the admin edits to fix it.

## 6. RBAC

Verified against `origin/main`, `deploy/k8s/rbac.yaml:27-37`: the `canyon-executor` Role in
`canyon-apps` gains one rule:

```yaml
- {apiGroups: [gateway.networking.k8s.io], resources: [httproutes], verbs: [get, create, update, delete]}
```

Nothing is added for `gateways.gateway.networking.k8s.io`, in any namespace, and no ClusterRole
changes: the executor never reads or writes the org's `Gateway` object. §5's wait reads the
`HTTPRoute`'s own `status.parents[].conditions` — a condition the Gateway's controller writes onto
the route it owns — so `get` on the route it already created is enough. Route status is enough;
the Gateway stays the org's.

## 7. Sign-in

Choice 2 — Canyon's own oauth2-proxy sidecar — runs its own OIDC round trip regardless of what
sits in front of it, so it works behind any Gateway. Choice 1 works wherever the Gateway itself
authenticates and forwards a verifiable OIDC token as `Authorization: Bearer` (an Envoy Gateway
`SecurityPolicy`, an Istio `RequestAuthentication`, or equivalent) — the sidecar's check is
unchanged (`contracts/executor.md:408-410`). Choice 1's `alb` is refused under `route.via:
"httproute"` (§3): an AWS load balancer's OIDC action is one listener rule, and mode A (`route`
absent) is where that rule already lives.

## 8. Limits

- **AWS.** The AWS Load Balancer Controller's Gateway API support (`gateway.k8s.aws/alb`) turns
  each `HTTPRoute` hostname into one ALB listener rule: one rule per app environment under B,
  against the default quota of 100 rules per ALB (raisable by service-quota request). Mode A uses
  one rule, total, no matter how many apps run.
- **Cilium's own Gateway.** Its data-plane traffic carries Cilium's reserved `ingress` identity,
  which no standard NetworkPolicy peer can name — `{namespace, labels}` and `{cidr}` both fail it —
  and naming it needs `CiliumNetworkPolicy`, which §9 leaves out. A cluster running Cilium's own
  `GatewayClass` uses mode A or C, not B.
- **Proven vs standard.** Envoy Gateway is proven in this repo's kind CI (§10); the AWS Load
  Balancer Controller is proven on the AWS test cluster. Every other conformant implementation
  (Istio, GKE, NGINX Gateway Fabric, Kong, Traefik, Calico, kgateway) is expected to behave the
  same way; none has been run against Canyon.

## 9. Deliberately left out

cert-manager, TLS termination in Canyon's own router, a `LoadBalancer` Service anywhere in the
base manifests, `CiliumNetworkPolicy`, any vendor CRD (AWS `ListenerRuleConfiguration`, GCP
`GCPBackendPolicy`, Istio `VirtualService`), and waiting for `Accepted` on every `/run` — only the
probe, on save, waits; a deploy writes its `HTTPRoute` and moves on, the same way it does not wait
for the router today.

## 10. Implementation plan

Four PRs, each off `main`, each ≤500 lines, contracts first.

1. **Contracts** — `contracts/schema/front-door.schema.json` (`route`: `via`, `gateway`, `from`),
   `contracts/schema/executor.schema.json` (`front_door`, `kube_objects.http_route`,
   `probe_result.route`), `contracts/routes.md:279`, `contracts/executor.md:388-398`, new goldens
   under `contracts/testdata/front-door/`, `contracts/validate.py`.
2. **Executor** — `executor/kube/objects.go` (`networkPolicy`'s ingress `From`, the `HTTPRoute`
   constructor), `executor/kube/run.go` (write it in `write()` beside the Service; delete it in
   `remove()` beside the Service and NetworkPolicy), `executor/kube/probe.go` (the `route` check),
   `go.mod`/`go.sum` (`sigs.k8s.io/gateway-api`). Unit tests beside each changed file.
3. **Deploy + kind** — `deploy/k8s/rbac.yaml` (§6), the kind CI setup installing Envoy Gateway and
   a fixture `GatewayClass`/`Gateway`, `deploy/k8s/deploy_test.go` extended for the new manifest,
   and two failing-first tests:
   - `TestKindHTTPRouteReachesTheAppThroughItsSidecar` — a real app pod behind a real `HTTPRoute`
     on a real Envoy Gateway in kind, reached end to end.
   - `TestKindProbeSeesAnUnattachedGateway` — the probe run against a `route.gateway` whose
     listener does not allow `canyon-apps`, asserting `not_accepted` and the fix sentence.
4. **Server + Settings UI** — `cmd/canyon-server/routes_front_door.go` (the new fields, the new
   refusals), `web/src/screens/Settings.tsx` (the Routing radio — wildcard/httproute — and the
   `route.gateway` and `route.from` rows, shown only under `runtime: kubernetes`).
