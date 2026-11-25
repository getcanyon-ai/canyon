# Egress — what an app calls out to, and with what

**Layer 1 part A built** (#2085, #2092, #2093: the contracts, the server's receiver, records, caps
and gaps, `via` and the recorded spec, the reads; records redesigned as one row per destination, #2222,
§2.4), the floor's namespace holder in compose (#2087) and Envoy in its namespace, sending to the receiver (#2090),
and the Docker executor's floor, slots, proxy variables and `POST /callers` (#2095): an app deployed under
`via` `canyon` calls out through Envoy alone, and one whose network predates egress is refused, never
started past the floor, until an admin's Recreate networks moves it across, either way (#2254); and the
Kubernetes executor's floor, Envoy and its own policy in `deploy/k8s/egress.yaml`, the proxy variables on
three containers and `POST /callers` (#2097, PR 7's first half); and the server's half (#2286): `PUT
/api/v1/egress` takes `canyon` on a host that names Envoy, Docker or Kubernetes, and the receiver runs on
Kubernetes too, the chart rendering the gateway as `deploy/k8s` does. Layer 2: the policy and request routes
(#2110), and the policy egress-control pulls, its version, callers, replicas and the deploy's wait (#2113, §3.2);
egress-control itself — the check, its 30 s of static stability, the rate limit's share, the denial log — and the receiver
taking the check's caller and outcome and filing egress requests (#2116), and the platform destinations carrying the
endpoints the front door's discovery names, which turned on refusing `not_allowed` (#2344, §3.3), and egress-control
reporting what Envoy lost (#2374, §2.4). Layer 3: its contracts (#2135), and the server's value routes, from-secret, the
route-value fetch, the interception root and each deploy row's trusted roots (#2137), and the intermediate, the
leaves egress-control fetches, an admin's sub-CA and the root's rotation (#2704), and egress-control serving the one
internal listener and its leaves by LDS and SDS over ADS, from the policy (#2139, §4.2), and egress-control's
interception: ext_authz #1's intercept, absolute-form and `inject_requires_tls` answers, ext_authz #2 and the
route-value cache (#2140, §4.1), and Envoy's side of it: its subscription to that listener, RBAC first on `:3128`,
`set_filter_state`, the intercept route, the internal cluster and upstream TLS that never resumes (#2141, §4.2), and
the inner access log: each inner request a call of its caller's row, `intercepted` (#2740, §2.4). The rest is
designed, not built; §13 is the PR list.

**The rule this document exists to enforce: an app reaches the internet through one door — an
Envoy Canyon configures — and what is on the other side is decided in Canyon, never in the
repository under review.** An agent that edits the repo cannot widen its own reach, an app never
holds a credential the door can add for it, and turning egress on never breaks Canyon itself.

**Canyon is the control plane, Envoy is the data plane.** Envoy (stock upstream image, pinned)
moves the bytes and terminates TLS only where a credential must be added. Canyon records, decides
and holds credentials. Canyon writes no proxy: enterprises already run Envoy, and a TLS-intercepting
proxy is security-critical code Canyon must not own.

| Layer | Gives | Brings | First useful |
|---|---|---|---|
| **1 · See** | every destination an app env calls: first and last seen, calls, the last result | Envoy static bootstrap, access logs (ALS) to the server, the floor | ~3,750 lines, 9 PRs |
| **2 · Restrict** | `observe` / `allowlist` per app env; refusals become egress requests | ext_authz: `egress-control` as a sidecar in the Envoy pod | ~4,400 lines |
| **3 · Inject** | the door adds a stored credential; the app never holds it | interception for inject destinations only; xDS; the pod's own token | spike passed (§13) |
| **Later** | E2 chained to the org's proxy · E3 the org's Istio · E4 the org's egress only · Layer 4 identity outward (JWT-SVID, SPIRE) | each its own spike and trigger, §5 | roadmap |

## How it works

**Glossary.** *Destination* — what an app calls (`api.stripe.com`); never "host", which in Canyon
is a Canyon server (design/OPERATIONS.md §2). *Host setting* — one per Canyon server. *The floor*
— the network rules that leave an app nothing to reach but Envoy and its database. *ALS* — Envoy's
gRPC access-log service. *ext_authz* — Envoy's gRPC check, asked before a request moves.
*egress-control* — `canyon-server egress-control`, Canyon's check beside Envoy (the access logs go to the server's receiver, §3.1).
*Platform destination* — an endpoint Canyon's own sidecars need (§3.3). *Inject destination* — one
with a route (§4). *Caller* — `(app, env)`, the path of its SPIFFE ID (§2.3).

### Life of one call — E1, pass-through, layer 2

```
 app pod (claims-portal prod)        canyon-egress pod                         internet
 ┌──────────────┐  CONNECT     ┌──────────────────────────────────┐
 │ app          │─api.stripe──▶│ Envoy :3128                       │
 │ HTTPS_PROXY= │  .com:443    │  ① ext_authz ──▶ egress-control   │  (localhost gRPC)
 │ canyon-egress│              │     ◀── OK + metadata | 403 | 503 │
 └──────────────┘              │  ② DFP resolves (public only)     │──TCP──▶ api.stripe.com:443
        ▲   ③ 200 established  │  ④ bytes spliced, never decrypted │◀──────── app's own TLS
        └──────────────────────│  ⑤ ALS entry (60 s flush, close)  │
                               └───────────────┬──────────────────┘
                                               │ ⑥ ALS stream, flushed every 10 s
                                               ▼
                                   canyon-server's receiver → egress_destinations
```

| # | Hop | Fails closed as | Proved by |
|---|---|---|---|
| 0 | The app's client honours `HTTPS_PROXY`; one that does not dials direct and the floor drops it | no route out | `TestEgressE2EPinsTheAppAndLeavesItNoWayOutButTheDoor`, `TestEnvoyCallOfAnAppRoutedViaCanyonIsItsRow` (Docker), `TestKindAppReachesOnlyEnvoy` |
| 1 | ext_authz names the caller by source address (§2.3), canonicalises the destination (§3.3), applies the mode | no answer → `503` + body; unknown caller → `403 unknown_caller` | `TestControlDownAnswers503WithBody`, `TestUnknownAddressDenied` |
| 2 | Envoy resolves; its own floor admits public 80/443 only | private, link-local, loopback → refused by the network | `TestConnectIMDSRefused`, `TestConnectCanyonServerRefused` |
| 3–4 | `CONNECT` answered, bytes spliced | — | `TestPassThroughDestinationIsNeverDecrypted` (layer 3) |
| 5–6 | ALS entry carries the check's metadata; the server's receiver drops every field outside the record schema, takes the check's caller and outcome, aggregates, files egress requests (#2116) | receiver down → buffered by Envoy, then lost, as a gap counted or no one counted (§2.4) | `TestALSPathNeverStoredOrLogged`, `TestTheCheckNamesTheCallerAndTheOutcome`, `TestEnvoysDroppedCountIsAGap` |

In layer 1 step ① does not exist: Envoy allows every public destination and the server attributes
each ALS entry afterwards (§2.3). That is the whole difference between See and Restrict.

### Life of one call — E1, inject, layer 3

```
 app ─CONNECT─▶ forwarder :4184 (canyon-identity) ─CONNECT + Proxy-Authorization: <pod token>─▶ Envoy :3128
   Envoy: ⓐ RBAC rejects any request already carrying x-canyon-intercept — before ext_authz
          ⓑ ext_authz #1: token → caller; route exists → OK: set x-canyon-intercept, dynamic metadata
             caller, remove proxy-authorization; any other OK removes x-canyon-intercept too.
             set_filter_state copies the caller to canyon.caller, shared with the upstream (ONCE)
          ⓒ route (connect_matcher and x-canyon-intercept) → the one internal cluster → the one internal
             listener; tls_inspector; chain server_names=[dest]; leaf by SDS (one SAN);
             ALPN h2,http/1.1 — the app's TLS ends here
          ⓓ header_mutation overwrites x-canyon-caller from canyon.caller and x-canyon-destination from
             canyon.destination → ext_authz #2 (context_extensions {destination}): caller empty refused;
             x-canyon-destination and :authority must be the destination (else 421); value fetched once and
             cached → OK: overwrite the header, remove x-canyon-caller and x-canyon-destination
          ⓔ upstream TLS re-originated (auto_sni, auto_san_validation, max_session_keys 0) ─▶ api.stripe.com
          ⓕ one ALS entry for the inner request, attributed by canyon.caller: method, status class,
             flags — the receiver drops its path
```

ⓐ forged header → `403` (`TestForgedInterceptHeaderRejected`) · ⓑ no or bad token → `403
unknown_caller` (`TestKindCallWithoutTokenRefused`) · ⓒ no matching chain → connection closed;
expired leaf → handshake fails (`TestExpiredLeafFailsClosed`) · ⓓ no caller in filter state → `403
unknown_caller` (`TestInnerHopWithoutCallerRefused`); value unreadable → `502 inject_unavailable`;
plain `http://` to an inject destination → `403 inject_requires_tls` (`TestMisdirectedAuthorityDenied`,
`TestInjectRequiresTLS`) · ⓔ bad upstream certificate → `502` (`TestUpstreamVerificationNeverSkipped`).
An absolute-form `https://` request to an inject destination never reaches ⓒ: ext_authz #1 injects
on the outer listener (§4.2).

**E2** (later): step ② is replaced — Envoy forwards the `CONNECT` unterminated to the org's proxy,
resolving nothing. **E3** (later): the app's mesh sidecar routes to a Canyon-dedicated Istio egress
gateway; that gateway's Envoy asks Canyon's ext_authz with the mesh certificate as the caller; See
and Restrict only (§5).

## 1. What is true today

Verified against `origin/main` at `48f8b11e`, by symbol:

- **Kubernetes.** `networkPolicy` (`executor/kube/objects.go`) gives an app pod `dnsEgress`, TCP
  5433 to the database ranges and `publicEgress(80, 443)` — every address outside `private`.
  contracts/executor.md, *On Kubernetes*, states the same. Nothing records what it calls. The
  `canyon-identity` sidecar fetches the front door's key set at start (`verifierOf`; ALB keys from
  `albKeysAt`), trying again for up to 10 s before it gives up (`startVerifier`, #2370), and oauth2-proxy calls the sign-in issuer (`deployment`, choice 2): both leave
  through the same per-pod policy.
- **Docker.** `createNetwork` makes `canyon-app-<app>` with a subnet, gateway and `canyon.app`
  label and no `--internal`: an app container reaches anything the machine reaches. **Amended
  2026-10-26 (#2503):** no longer anything — each app container on an open network runs in its own floored
  holder's namespace, which refuses the host (the gateway, link-local, the metadata addresses), Canyon's
  other networks and every private range but `CANYON_APPS_ALLOW`'s, and admits every public address
  (contracts/executor.md, *An app reaches no private, link-local or host address*). Recording and limiting
  what it calls stays via `canyon`'s. `ensureNetwork`
  joins `CANYON_EXECUTOR_JOIN`'s containers to every app network. `Run` starts the container with
  `canyon.app`/`canyon.target` labels and every capability dropped. The app's
  `CANYON_IDENTITY_JWKS_URL` is `http://canyon-server:7780/…` (contracts/executor.md, `/run`).
- **Secrets reach the app as environment** (`/run`'s `env`; on Kubernetes `envSecret`). No route
  reads a vault value back (contracts/routes.md, the secrets row): the app holding the key is the leak.
- **Pods.** `deployment` mounts no service-account token and no service links and labels the pod
  `canyon.run` with the executor's own nonce. Only `canyon-executor` holds a Role in `canyon-apps`
  (`deploy/k8s/rbac.yaml`), with `list` on pods. The server runs one replica (`deploy/k8s/server.yaml`).

## 2. Layer 1 — See (access logs only)

No ext_authz, no egress-control, no state pull, no xDS, no forwarder: a static Envoy, a floor, and
a receiver in the server.

### 2.1 Envoy, static

`canyon-egress` runs `docker.io/envoyproxy/envoy` pinned by digest (`CANYON_IMAGE_EGRESS` overrides
it, §7) with `deploy/egress/envoy.yaml`, checked in CI by `envoy --mode validate`:

- **Listener :3128**, HTTP connection manager speaking HTTP/1.1 alone (`codec_type: HTTP1`, as proxy
  clients do: h2c's one connection would carry streams without end, past every bound counted in
  connections) with `CONNECT` upgrades; `dynamic_forward_proxy` filter and cluster
  (`dns_lookup_family: V4_ONLY`, `max_hosts` 16,384); `CONNECT` is terminated
  and tunnelled to its authority on 443; absolute-form `http://` on 80 and absolute-form `https://`
  (what axios ≤ 1.7 sends instead of `CONNECT`) are proxied with Envoy originating TLS upstream —
  a route matching `:scheme https`; Envoy takes `:authority` from the URL and drops `Host`.
  `CONNECT` over HTTP/1.0 is answered `426` (`accept_http_10` stays false).
- **Hardening:** `skip_xff_append`, `generate_request_id: false`, `x-request-id` and
  `x-forwarded-proto` removed (route `request_headers_to_remove`; `suppress_envoy_headers` leaves
  both), `suppress_envoy_headers` on the router; `max_stream_duration` 15 min on every stream; the
  overload manager (heap, active connections: 16,384 in all); each upstream cluster's circuit breakers,
  and the DNS cache's, at the same 16,384 connections, requests and pending ones, never Envoy's default
  1,024, which a few apps' tunnels would fill for every app; and a per-source connection cap, which
  stock Envoy has none of, so the holder's kernel enforces it in Envoy's namespace: one source address
  holds at most `CANYON_EGRESS_PER_SOURCE` connections to :3128, default **512**, and its next is reset
  (`iptables` `connlimit` on `INPUT`, `--connlimit-mask 32`, `REJECT --reject-with tcp-reset`; #2239).
  An app's container is one source address and its process holds 1,024 files at most (the executor's
  `nofile`): 512 is half of one process's and a 32nd of Envoy's 16,384, so no one app holds the rest out.
  On Kubernetes there is no holder and no per-source cap (the owner's decision, 2026-10-16: no `NET_ADMIN`
  in Canyon's namespace): a connection with no request open — one that never sends a byte among them — is
  closed after 60 s, and as a replica's connections pass half of its 16,384 the overload manager shortens
  that, no lower than 5 s, reaching it at 90%. **The residual risk, stated:** nothing caps one pod —
  no per-source cap, and no `nofile` limit per pod as Docker's executor sets — and the Service spreads its
  connections over both replicas, so one app pod that holds `CONNECT` tunnels, each moving a byte within
  every 5 minutes (the stream's idle timer, which is never scaled) and opened again at its 15-minute cap,
  fills both replicas' 16,384 and denies egress to every app on the cluster, not one replica's share of it;
  kept-alive connections alone would need a request within every 5 s near the cap, where the idle timer
  is scaled down. The exact cap is #2287.
- **Admin on a Unix socket** (`/var/run/envoy-admin/admin.sock`, mode 0600) — never a TCP port, and on a
  volume of its own, in memory, that Envoy and egress-control mount and nothing else does, read-only to
  egress-control, which reads what Envoy lost through it (§2.4; #2374). Whoever reaches the socket reads the
  bootstrap, the receiver's token in it (`GET /config_dump`), and POSTs admin calls, and a read-only mount limits
  neither, so what guards it is that only those two reach it: no app shares that volume or Envoy's pod.
  **No file or stdout access log.** **ALS** (`envoy.access_loggers.http_grpc`) to the server's internal gRPC
  receiver (in layer 2 too: each entry carries the check's metadata, which the receiver reads — #2116),
  `access_log_flush_interval` 60 s so a long
  tunnel is attributed while its caller lives, and `flush_log_on_tunnel_successfully_established`, so a tunnel is
  its row from the moment it opens (§2.4, #2684).
- **Every route names its port** — 443 for `CONNECT` and absolute-form `https://`, 80 for `http://` —
  and anything else is no route, `404`: the server's receiver is the one port of Canyon's networks the
  floor admits from Envoy's namespace, so an app never asks Envoy for it (#2090).
- **A call has 15 minutes, as a tunnel does:** the absolute-form routes' timeout is 900 s, not Envoy's
  default 15 s, which a model API's slow answer outlives (#2090), and every stream ends at 900 s
  (`common_http_protocol_options.max_stream_duration`), a request whose body trickles in among them,
  which the route's timeout, counted from the request's last byte, never ends. **Idle is bounded:** a client's headers
  must arrive within 10 s of their first byte (`request_headers_timeout`); a request or tunnel that moves
  no byte for 5 minutes is closed (`stream_idle_timeout`), above a long poll's minutes and a websocket's
  pings, every 60 s at most, and any byte of a slow upload restarts it; a connection with no request open — a
  client's kept-alive one between requests, or one that never sends a byte, which no request timer
  bounds — is closed after 60 s (`common_http_protocol_options.idle_timeout`, Envoy's 1 h unset), as
  an AWS load balancer's is; and from half of Envoy's 16,384 connections the overload manager scales that
  last one down, no lower than 5 s, reaching it at 90% (`reduce_timeouts`), so idle connections go first —
  never a stream's timer, which a byte every few seconds beats and which, scaled, would cut every app's
  silent model-API call while one app floods (#2287).
- **Filled in at start:** `deploy/egress/envoy.sh` writes the receiver's address, Envoy's token
  (contracts/routes.md, Egress control) and the resolvers from the environment into the bootstrap, on
  a tmpfs only Envoy reads, and starts Envoy on it, `--concurrency` `CANYON_EGRESS_CONCURRENCY`, 8 unset (four on Kubernetes, §2.2); CI validates what it writes. A
  token outside the server's own rule (32–512 of `[A-Za-z0-9._~+/=-]`), a resolver or receiver that is
  no IPv4 address, or no resolver at all stops it before anything is written (fail closed).
- **Not every call of a floored pod passes it** (Kubernetes alone, #2316): under
  `CANYON_KUBE_SIGNIN_DIRECT` a floored pod's sidecars reach the listed sign-in hosts directly, never
  through Envoy, and the whole pod reaches their listed addresses on 443, so no record holds those calls
  (§2.2).

### 2.2 The floor — and never breaking Canyon

- **Docker.** New app networks are `--internal` with `--ip-range <the /24's upper /25>`, their
  gateway isolated (`com.docker.network.bridge.gateway_mode_ipv4=isolated`, Docker 28.0+: no address of
  the host's on the network, so a service the host runs on every address — a natively run executor's
  `:7778` — is out of reach; amended 2026-10-15, #2095); the container `canyon-egress` is joined to each,
  and to no open network, through `CANYON_EXECUTOR_EGRESS_JOIN`, so a holder that is down fails only the
  deploys routed through it. `canyon-egress` is a
  **namespace holder** (pinned stock image with `iptables`, `NET_ADMIN`, no listener;
  `CANYON_IMAGE_EGRESS_HOLDER` overrides it, §7): it applies, in its own network namespace, `REJECT`
  on new outbound connections to RFC 1918, 100.64/10, link-local, loopback, Canyon's pools
  (`CANYON_EGRESS_POOLS`: the apps' pool and Canyon's own networks), 0.0.0.0/8 (where OrbStack puts
  `host.docker.internal`; amended 2026-10-27, #2503), 192.0.0.0/24, 198.18.0.0/15,
  240.0.0.0/4 and Azure's WireServer (168.63.129.16) — except the configured upstream resolvers on
  53 (a resolver in Canyon's pools or on loopback is refused all the same; the WireServer is
  admitted on 53 when configured, as Azure VMs resolve through it) and the server's receiver — and
  on every public destination but TCP 80 and 443, and before those on every address of the Docker host's own
  (#2524, amended 2026-10-27): the compose service `egress-host`, in the host's network namespace with no
  capability, removes the last listing and writes `ip addr show` under the kernel's `boot_id` and over an `end`
  line into a volume the holder reads at its start, never again (an address the host takes later is refused once
  egress starts again), and a listing missing, unreadable, of another boot, cut short (its last line not `end`) or
  naming no address but loopback stops the floor before its first rule; it turns forwarding off (`ip_forward` 0,
  `FORWARD` dropped) and IPv6 with it (a host booted with `ipv6.disable=1` cannot start it), caps
  each source's connections to Envoy (§2.1), then sleeps; Envoy runs in that namespace (`network_mode: service:`) without `NET_ADMIN`. Envoy's DFP resolves through `CANYON_EGRESS_RESOLVERS` (the machine's upstream
  resolvers; compose defaults to `8.8.8.8,8.8.4.4`, the public ones Docker itself falls back to), never Docker's `127.0.0.11`, which would answer `canyon-server`. Envoy and the
  server's receiver share a dedicated `canyon-egress-ctl` network — never `canyon-internal`, where
  the executor holds the Docker socket. Envoy needs Docker 28+, as `ensureNetwork`'s
  `--gw-priority` already does.
- **Docker, via `off`** (#2503, 2026-10-26). An open network still routes out through the host, so each
  app container on one runs in a holder's namespace, `canyon-appfloor-<nonce>`, the build holder's image and
  pattern (contracts/executor.md, *An app reaches no private, link-local or host address*): its floor
  rejects the network's gateway, every address of the Docker host's own (read from the host's namespace
  as the holder starts, so a public one too), link-local, the metadata addresses, loopback and the unroutable ranges
  before the app's own /24 returns, then Canyon's other networks, then `CANYON_APPS_ALLOW`'s entries
  return, then every private range is rejected, and every public address on any port returns, as before.
  Under `demo` that closes the loop through the host's own forward to its loopback publish
  (`host.docker.internal` on Docker Desktop, OrbStack, Colima), which every check on the server's side
  reads, correctly, as this machine; under `oidc` it keeps apps off the host and its LAN. Docker
  restarts no holder, so the executor brings each holder, then its app, back in order (*Reconcile*). An
  app deployed before the floor runs unfloored until its next deploy: redeploy every app once after
  upgrading; the executor logs each one until then.
- **Existing Docker networks** are not internal and Docker cannot change that. Turning egress on
  lists them; an admin's **Recreate networks** (one executor call per app) stops the app's
  containers, recreates the network, and starts them again from their deploy rows — a stated
  per-app outage of seconds. Until then the app's records say `floor: not enforced`, and the executor
  refuses a deploy of the app routed via `canyon` (`409`, contracts/executor.md, Egress) — never an app
  started past the floor.
- **Only Recreate networks moves an app across the floor, both ways** (the owner's decision, 2026-10-15,
  #2254). Under via `canyon` it makes an open network internal; under via `off` it makes an internal one
  open, and turning egress off lists those the same way: until then their apps keep calling out through
  Envoy. A deploy never changes a network (§2.5). The server sends the executor `internal` — the host's
  via is `canyon` — and the executor stops the app's containers and recreates the network on its own /24.
  Before the call the server records the network as **unknown**, in a transaction of its own — read as
  not internal, so the app is listed to recreate and its floor is never `enforced` while the executor may
  have changed it — and once the executor answers, records it internal or open, before any env starts
  again; then it starts each env again with `/run` from its deploy row, production first so its outage
  is the shorter, its image and today's runtime values under the host's via; the row records that spec. A
  failure after the stop leaves the app **stopped**, never running on an open network again; recreating
  again repeats the whole of it. A `/run` a deploy actually makes records the network too where it is not
  what the record says, as the executor starts `canyon` on an internal network alone and `off` on an open
  one alone; a retry that finds its version already running makes none, records nothing, and keeps on its
  deploy row the spec its first attempt started under, never one it did not run. While Canyon runs an env
  of the app the server reads its network from that record alone, and an app with none reads open (fail
  closed) — the truth for every app from before recreation, as via `canyon` was refused on every host. One
  recreation runs at a time on a host; an app whose production is held, or which is being deleted, is never
  recreated, and an env is not started again once its app is marked deleting; a hold, an undeploy or a
  delete of the app waits behind a recreation under way for 20 s at most, then answers `409 busy` — no
  waiter holds a database connection without end — and the delete stops what the recreation started. Settings › Outbound lists the networks to
  recreate, each with its Recreate, and the app's Outbound card and `canyon status` say a `floor` that is not `enforced` (#2099).
- **Kubernetes.** `networkPolicy`'s egress becomes kube-dns, TCP 5433 to the database ranges and
  TCP 3128 to `canyon-egress` pods; `publicEgress(80, 443)` goes. Envoy's own NetworkPolicy admits
  public 80/443 (the `private` ranges and 169.254/16 excepted — IMDS with a hop limit of 1 is out of
  reach either way), kube-dns and the server's receiver. Built (#2097): a `/run` without `egress` — via
  `off` — keeps `publicEgress(80, 443)`; `canyon-egress` is two replicas of four workers each, two access-log streams a
  worker, so a rollout's three, 24 streams, stay under the receiver's 32 (`CANYON_EGRESS_CONCURRENCY`, §2.1; #35); before a `/run` with
  `egress` writes anything the executor opens TCP to the gateway, which proves Envoy answers — the app's
  own path is `TestKindAppReachesOnlyEnvoy`'s; a failed `/run` puts the app's previous policy back with
  its template, a wider one only once the failed rollout's pods are gone, else the narrower stays (fail
  closed); the proxy variables are literal values of the template and a floored pod is labelled
  `canyon.floor`, which every other policy of `canyon-apps` leaves out, but one to an internal identity
  provider (below); the private ranges, for apps and
  Envoy alike, are Docker's floor's list, which assumes the cluster's pod and Service ranges are among them
  (#2297). A via flip cuts the replaced pods' way out for the whole rollout (#2298). No per-source
  connection cap: idle connections are closed instead, and one app pod can still fill both replicas and
  deny every app egress (§2.1, the residual risk; #2287). **IPv6:** Envoy resolves IPv4 only and
  every policy names IPv4 blocks alone, so on a dual-stack cluster IPv6 egress is denied and an
  IPv6-only destination is unreachable in v1. The front-door probe that refuses an unenforcing
  cluster gates every `/run` on Kubernetes — none runs without a front door, and none runs behind a door
  that has not passed the probe (#2438) — and so every app routed through Canyon: `PUT` `canyon` asks no probe of its own
  (#2286). **The receiver on Kubernetes** listens on `0.0.0.0:7781` and on no other address, IPv4 alone, no manifest
  knowing the pod's address,
  and takes Envoy's bearer token, the Secret `canyon-egress`'s `token`, which the server and Envoy both read;
  the server's NetworkPolicy admits 7781 from `canyon-egress` pods alone — a static token and the policy, as
  Docker's token and its dedicated network. `server.yaml` sets `CANYON_EGRESS_PROXY` to
  `http://canyon-egress.canyon.svc:3128`, and the Helm chart renders the gateway, its token and the server's
  half as `deploy/k8s` does.
- **An internal identity provider on Kubernetes** (the owner's decisions, 2026-10-17, #2316: a direct path,
  by host). Envoy refuses private addresses, so a floored app's sidecars could not reach a front door whose
  provider is internal, and nobody could sign in to the app. The executor's `CANYON_KUBE_SIGNIN_DIRECT`
  lists the sign-in hosts that go direct, each `<host>` or `<host>=<address>/32`: a lower-case DNS name or
  IPv4 address, and one IPv4 address outside link-local; anything else and the executor does not start
  (fail closed). With it, a floored pod's sidecars' `NO_PROXY` carries each front-door sign-in host the
  setting lists — choice 2's `signin.issuer`, choice 1 `oidc`'s `issuer` and `jwks_url` — and no other,
  so a public provider keeps going through Envoy with the setting on; and every floored pod's
  NetworkPolicy admits TCP 443 to each listed address. Any front-door URL on a listed host naming another
  port than 443 is refused at `/run`. `NO_PROXY` matches a listed host's subdomains too, which still reach
  only the listed addresses on 443. A provider **outside the cluster** is listed with its address. A
  provider **inside the cluster** is listed without one: a Service's or load balancer's address is
  rewritten to a pod's before policies match, so no `/32` reaches it, and the operator's own policy, beside
  Canyon's, admits floored pods to the provider's own pods, on the port they serve (the docs give it, and the
  Helm chart renders it from `canyon.egress.signin.inCluster`, refusing any namespace the chart writes or
  installs into and `kube-system`; #2326). That
  is only for a provider whose sign-in host resolves to its own Service: a policy to an ingress's, a
  gateway's or a load balancer's pods would give every floored app all they serve, unrecorded. Every back-channel endpoint the sidecars call —
  `token_endpoint`, `jwks_uri` — must be on a listed host. **The costs, accepted:** the whole pod, the app's
  own container included, reaches the listed addresses on 443 (or, inside the cluster, the provider's pods
  on the port the operator's policy names); and those calls pass no Envoy, so none is recorded (§2.1, §10). The policy and the variables are
  written by each `/run`, so adding, narrowing or removing the setting reaches a running app only at its
  next deploy — until then a removed address stays open to it; the sidecar roll (#2319) changes
  `canyon-identity`'s image alone.
- **A sign-in served through the cluster's own gateway is not supported under via `canyon`, for now**
  (the owner's decision, 2026-10-17, #2316). The Helm chart's Pocket ID on k3s, or an operator's Dex, Keycloak or
  Zitadel behind their own gateway, is reached at the gateway's address, which kube-proxy rewrites to the
  gateway's pod address — a private one Envoy refuses and no `/32` names, and whose pods serve everything
  else too. The server's `CANYON_KUBE_GATEWAY_SIGNIN` names that provider's issuer: the chart sets it on k3s
  to its Pocket ID, the k3s guide has an operator set it, and the chart opens no gateway path for floored
  pods. While the front door signs in at it, `PUT` `canyon` is `409` with D5's line (another sign-in
  provider, or egress off), read under the lock a front-door save takes, and the admin read's
  `canyon_refused` says it in Settings › Outbound; a front door saved to it under via `canyon` is never
  refused — that could lock admins out — but answers D5's warning (`egress_note`). Revisit if someone
  asks.
- **Every kind.** The floor is per app pod, not per web process: a scheduled job's run
  (design/JOBS.md) gets the same egress rules, and its NetworkPolicy's empty ingress is
  unchanged by this design.
- **Canyon's own calls.** The proxy variables (§4.4) are set on `app`, `canyon-identity` **and**
  `oauth2-proxy`. `NO_PROXY` is derived from every in-network URL the executor writes — the JWKS
  server (`canyon-server` on Docker, `127.0.0.1` on Kubernetes), the database host, `localhost` —
  so token verification never reaches Envoy; on Kubernetes the sidecars' also names each front-door
  sign-in host `CANYON_KUBE_SIGNIN_DIRECT` lists (above). In layer 2 the front door's endpoints are
  platform destinations, always allowed (§3.3).

### 2.3 Attribution

Which app made a call is **one small step**, run once per flush on the calls an entry became, and
**chosen by the listener the stream arrived on** — never by anything in an entry an app can write. Today
there is one listener and one step, by address; the step's seam — a listener tag on each call, a
`(app, env)` answer — arrives with the second, by workload (#2231). Its
answer is a caller `(app, env)` or none; a call with none is no row and is counted `unattributed`
(§2.4). `(app, env)` is the path of the SPIFFE ID Layer 4 keeps as the contract,
`spiffe://<canyon trust domain>/canyon/<app>/<env>` (§5): every step answers in that one name, so a
new source adds a step, never a second notion of caller.

| Step | Listener, and how its stream is proved | The caller from | Where |
|---|---|---|---|
| **by address** (built) | Canyon's own: the server's address on `canyon-egress-ctl`, Envoy's bearer token (contracts/routes.md, Egress control) | the entry's direct peer, `downstream_direct_remote_address`, which the executor's `POST /callers` names | Docker; Kubernetes with Canyon's Envoy (#2097) |
| **by tunnel** (built, #2740) | Canyon's own, as by address; the stream's log, which Envoy names in its first message: the interception listener's, `intercept`, never `:3128`'s `egress` | the entry's filter-state object `canyon.caller`, which ext_authz #1 named for the tunnel: an inner request's source is Envoy's internal address, no app's (§4.2) | wherever layer 3 runs |
| **by workload** | a mesh listener: mTLS whose client certificate the mesh's root signs. Istio's `envoyHttpAls` provider sends no token, so the certificate is the proof | the stream's peer SPIFFE ID (a sidecar's own outbound entries), or the entry's `common_properties.tls_properties.peer_certificate_properties.subject_alt_name[].uri` (a gateway's, E3) | Istio, later |
| **OpenTelemetry** | Istio's `envoyOtelAls` provider: OTLP `LogsService/Export` on a mesh listener as above | as by workload; the call's fields from the attributes Canyon's provider names in its `logFormat.labels` (never `%REQ(:PATH)%`), every other attribute dropped on receipt | later, through the same step |

**By address**, the runtimes:

| Runtime | Address → caller |
|---|---|
| **Docker** | the /24 → app (the network's `canyon.app` label); fixed slots in the lower /25: web preview `.10`/`.12`, web prod `.11`/`.13` (the redeploy's previous container holds the other), job preview `.14`, job prod `.15` (JOBS.md §6); dynamic addresses only in the upper /25 |
| **Kubernetes** | the pod IP, resolved by the executor's new `POST /callers {addresses[]}`, which lists pods in `canyon-apps` (it already holds `list`); an IP claimed by more than one pod, or by a terminating pod, is `unknown` |

`/run` gains `run` (the server's run id), which the executor writes as `canyon.gate_run`; `/job`
already carries it as `canyon.run`; `POST /callers` answers it, and the records keep none (§2.4, §14).
The server resolves each flush's addresses once and caches nothing across flushes. A call the check named
a caller for is that caller's; for any other, `POST /callers` answers each holder's `since`, a time no
earlier than when it took the address — on Docker its container's `State.StartedAt`, on Kubernetes the
second past the earliest `startedAt` of its pod's running containers — and the receiver names the holder
only for a call Envoy says started at or after it (#2265). A call a gone container or pod made, its entry
read once another app holds its address, is no one's, counted as unattributed and never another app's;
so is a call whose start Envoy did not say. The address passed from one holder to the next in between, so
no call the last one made started after the next one's `since`. The cost, stated: a holder's calls before
its `since` are no one's too — a container's from before its last start, a pod's init containers' and its
calls from before a restart of its every container, and up to a second of a pod's first, its `startedAt`
kept to the second; the clocks compared are Envoy's and the docker daemon's on one host, on Kubernetes
Envoy's node's and the kubelet's; and layer 3, which injects secrets, does not rely on addresses on
Kubernetes (§4.3).

**By workload** (Istio; its sidecars are Envoy, and its Telemetry API points their access logs at
Canyon: `meshConfig.extensionProviders` `canyon: envoyHttpAls {service, port}`, a `Telemetry` in
`canyon-apps` selecting it). The first message of every ALS stream carries `identifier.node`: Istio's
`node.id` (`sidecar~<pod IP>~<pod>.<namespace>~…`) and `node.metadata` `NAMESPACE`,
`SERVICE_ACCOUNT`, `WORKLOAD_NAME`, `LABELS`. That metadata is the sidecar's own say-so, so it is never
the caller alone: the caller is the mTLS peer's SPIFFE ID, `spiffe://<mesh trust domain>/ns/<ns>/sa/<sa>`,
and a node whose `NAMESPACE` and `SERVICE_ACCOUNT` disagree with it leaves the stream's calls
unattributed. The ID is matched **exactly** against the ServiceAccount the executor gives each app env,
`canyon-app-<app>-<target>` in `canyon-apps` (§4.3) — looked up, never parsed — so the step needs those
ServiceAccounts, which arrive with layer 3. How the mesh's stream reaches the receiver (a
`DestinationRule` with `ISTIO_MUTUAL`, the mesh root the receiver trusts) is the Istio kind spike's (§5).

### 2.4 Records

**One row per destination an app env calls** (#2222, the owner 2026-10-13): `egress_destinations`, key
`(app, env, destination, port)`, so the table stays small and Canyon never keeps Envoy's log.

| Column | |
|---|---|
| `first_seen_at` · `last_seen_at` | when the receiver took the first and the latest call's entry |
| `calls` | the calls since first seen, a tunnel once, as it opens |
| `last_outcome` | `allowed` (layer 1) · `refused` (layer 2) |
| `last_status_class` | `1xx`–`5xx`; `none` when the call got no answer |
| `last_protocol` | `connect` — a tunnel: bytes spliced, never read, its class the `CONNECT`'s own · `http` — a plain request · `https` — an absolute-form `https://` request Envoy sends on in TLS it originates · `intercepted` — layer 3: one request inside a tunnel Envoy ended to add a route's credential |
| `last_response_flags` | Envoy's own (`UF`, `URX`, `DF`, `NC`, `UAEX`…), sorted |

The four `last_` columns are **the last result**, always one call's together: an ended call's, or an open tunnel's on the row it created, its `CONNECT`'s `2xx`. **Not kept:** the caller's
kind (a web process's and a job run's calls are one row of their app env; nothing reads which), the
run, the method beyond `last_protocol`, bytes, durations, and any count by minute, hour or day — rates
are Envoy's own stats (§7). **Never:** a path, query, header value, body or resolved address — the
receiver drops every field outside the schema on receipt, before any aggregation, log line or write.
**That drop is the only guard on the path:** every non-`CONNECT` ALS entry carries `request.path` with
its query (an absolute-form request's, an intercepted inner request's), and
`envoy.access_loggers.http_grpc` has no option to leave it out (spike, §13) — so the receiver drops the
path and query on receipt, never later. The same holds for the ext_authz CheckRequest, which carries
them too: from layer 2 **egress-control never logs a CheckRequest**, whole or in part. An intercepted
call's entry (layer 3) has no app source address (the inner hop's source is `internal_client_address`);
it is attributed by the filter-state object `canyon.caller`, which the inner listener's ALS logs
(`filter_state_objects_to_log`), and nothing else of the tunnel's state (#2740). Its row is its authority's, as
every entry's, so a tunnel and its inner requests share one row, the destination's on 443: the tunnel counts one
call, as it opens, and each inner request one, and the last of them to end sets `last_protocol`, most often the
tunnel's own `connect`, as a tunnel ends after the requests it carried. A misdirected inner request is a refused
call of the name it asked for, as on `:3128`, the listener's own `421`, which asks no check, among them: an inner entry
is `allowed` only when ext_authz #2 said so, so no app makes a name known by asking for it inside a tunnel. `intercepted` widens 0084's `last_protocol` CHECK, in a migration of
its own: one row the CHECK refused would abort the flush, and every app's rows of it.

**A flush.** The receiver sums the flush's calls in memory, one entry per key: the calls, the earliest
and latest entry's time, and the latest call's result (a tie: the later in the batch). A periodic
entry counts nothing; it carries a tunnel's caller to its last entry. Then, in the flush's one
transaction with its gaps and heartbeat, **one upsert per key**: `calls = calls + n`, `first_seen_at =
least(…)`, `last_seen_at = greatest(…)`, and the four `last_` columns the incoming ones only when its
`last_seen_at` is not older. No partition: a call taken at 23:59:50 and flushed at 00:00:10 is a row
like any other (#2221).

**A tunnel is one call of two entries** (#2684). Envoy logs an entry when a call ends, up to 15 minutes after a
tunnel opens, and its buffer goes with it, so a destination reached only through a tunnel still open would have no
row while preview is connected to it. So Envoy logs a tunnel as it opens too, once its `CONNECT` is answered `200`,
and the row is written then. The open entry counts the call and moves `last_seen_at`, and writes the `last_`
columns only on a row it creates: a later tunnel's opening is no result, so a row's stay the last ended call's. The
last entry, a `CONNECT` answered `200` under the bootstrap's tag, counts nothing and writes the `last_` columns; with
no row, its open entry lost, it writes one of 1 call, and on a row it adds none, so a call whose open entry went on a
stream that broke, which no gap says, is one short there. A periodic entry between them, of such a `CONNECT`, moves
`last_seen_at` alone, so a tunnel opened at boot reads as seen within about a minute, not when it opened (§3.4): it counts nothing,
leaves the result and `admitted_at`, and with no row writes one of 1 call. Any other periodic entry writes nothing. No
state links the entries, so a receiver restarted between them counts the call once. Every entry of the bootstrap
carries the tag, so an entry without it is an Envoy from before
this, whose tunnels are counted at their last entry as ever; a receiver from before this counts each tunnel twice
while the new bootstrap runs, and nothing else differs. A call refused, or with no upstream, is never a tunnel: its
one entry counts it. An upgrade that is no `CONNECT`, a `GET` with `Upgrade: CONNECT` answered `101`, is logged as it
opens too: that entry writes nothing and its last entry counts it, so an app can't add to its own calls, and it is no
row while it runs. An open entry past the cap is counted on `over_cap` and one no app is named for on
`unattributed`, so both read as a gap while the tunnel runs, and its later entries add to neither, though those past
the cap move `over_cap`'s `to_at`, so it stays a gap for as long as the tunnel runs; a later entry past the cap with no
`over_cap` row yet, its open entry lost, writes one of 1 call.

**The cap: 500 destinations per app env** by default, an admin's to set from 50 to 5,000 (#2691), `invalid` not
among them; lowered below the rows an env holds, it deletes none, and every new destination is `over_cap` until
the env is under it. An app's own integrations are
tens, so 500 is an order of magnitude of headroom; it bounds the table at 1,000 rows an app and the
Outbound card at one read; an app past it is fanning out (webhooks to its users' URLs, a crawler),
where a row per name tells no one anything and the count is what matters. Per app env, not per app:
preview's experiments never crowd out production's rows. Each flush reads the keys of each app env
it touches (≤ 500 rows), caching nothing; a key already a row is upserted; a new key gets a row while
there is room, in the order first called; **every call to a new key past it is counted on that app
env's `over_cap` gap** — never a row, never silent. The cap holds with two writers at once (a second
replica, a restart while the last flush commits): a flush takes each of its app envs' advisory locks
before it reads their rows, and takes them, and writes the rows, in key order, so no two wait in a cycle. A
destination is the name as the check canonicalises it, or a dotted IPv4 address, at the port called (#2116):
any other IP-literal encoding, a host UTS 46 maps to another name, userinfo, a single label, a label over 63
characters, a name over 253, or a port written other than as its number (`0443`, `+443`) is `invalid`, on 443.

**Gaps** — a receiver down, Envoy's ALS buffer dropping (`logs_dropped`), a batch not written, calls
no one is named for — stay as they are, each counted (`egress_gaps`, contracts/schema/egress.schema.json),
with one reason more: `over_cap`, the only gap that names its app env, and **one row an app env**,
extended by every burst past the cap however far apart. A gap that names no app and continues the last
one written of its reason and countedness — starting where that row ends — extends that one row (never
two that end alike), so a day of `unattributed` calls is one row, not 8,640. A caller the executor names
with an app id out of contract, or a target other than `preview` or `prod`, is no one's.

**What Envoy lost** (#2374). An entry Envoy lost never reaches the receiver, so nothing on the server's side
can count it. egress-control reads Envoy's stats on the admin socket every 10 s, first 10 s after it starts, and
once more as it stops, and reports what they say (`POST /egress/dropped`, contracts/routes.md); the next flush
writes it as `envoy_dropped`, the same way as the receiver's own `dropped`. A flush that writes either, or a stream
the receiver refused or couldn't read, logs `egress: calls out may not be recorded: …`, at most once a minute. No
stream open is no sign by itself: Envoy opens one when it has a call to send, so an idle host has none (#146).
One read names three stats:

| Stat | What it says | Written as |
|---|---|---|
| `access_logs.grpc_access_log.logs_dropped` | entries a worker dropped: its 16 KiB buffer was full and the stream took no more | a rise is that many calls |
| `cluster.receiver.upstream_rq_pending_failure_eject` | streams to the receiver that failed to open. Envoy counts what it wrote to one as flushed and never as dropped, so the entries are gone and no counter has them (#2377) | a rise is a gap no one counted, `calls` `null` |
| `server.uptime` | how long this Envoy has run | below, with a counter that went backwards |

**A decrease is never a negative gap.** Only a start takes a counter backwards, and a start or a clock stepped back
takes `server.uptime` backwards: what Envoy lost around that no one counted, a gap with `calls` `null`, and everything the new Envoy counted is its count.
**An unread count is never none.** A read that fails, a stat left out, an answer that does not parse or a read
past 2 s, as Envoy's main thread stuck while its workers proxy on, is a gap no one counted; a read has 2 s and its
reports 5 s of their own, so one that hung still reports, inside Docker's 10 s stop. The next read that answers
still counts the rise across it, as the counter kept running. A first read that finds either counter above zero is
a gap no one counted too: egress-control keeps nothing across a start, so it cannot say how much an earlier one
reported. Two failed reads say nothing: no socket file in egress-control's first minute, before Envoy's first answer, is no
Envoy yet, as one that never started lost nothing, and past that minute, or once it has answered, a missing socket is
a read that failed, or an Envoy proxying with its socket elsewhere would read as nothing lost for as long as it ran; and a read egress-control's
own stop cut short says nothing of Envoy, the last read deciding. What the server does not take, egress-control
keeps, summed, and sends at its next read, and one that stops with a report undelivered leaves the counter above
zero for that first read to find. A read or a report that fails logs once a minute, the first a minute after the
start, so an Envoy slow to start logs nothing and a socket that never comes does.

**A healthy stop writes a gap, as intended** (decided 2026-11-06). Envoy and egress-control stop
together, so the last read finds Envoy gone and reports what no one counted, on every rollout of the gateway.
Envoy's buffer and its calls in flight really do go with it, and an honest gap beats a quiet loss.

**Not counted anywhere, stated:** an entry on a stream already open when it breaks; one in a worker's buffer, or a
call in flight, when the pod goes with Envoy and egress-control's last read fails to reach the server; and a `204`
held in memory when the server crashes within 15 s of its last flush, which leaves no `receiver_down` gap either.
A tunnel open when Envoy goes keeps its row and its call: only its last result goes, as its open entry left a
worker's buffer within a second of the tunnel opening.
Records are evidence: none of this stops a call, and an Envoy that cannot be read keeps serving (§6).

**Age.** A destination unseen for **30 days** is deleted, and a gap that ended 30 days ago with it:
two `DELETE`s by index, every minute, in the receiver's loop — no partitions, no roll-up. Each skips a
row a flush holds (`FOR UPDATE SKIP LOCKED`), which the flush sees again or the next minute's takes, so
a prune never waits on a flush, and never in a cycle with one. Rows age
because a dead destination would otherwise hold a cap slot forever and the card list what the app no
longer calls; 30 days is the gaps' keep too, so no row's last call is older than the gaps that qualify
it. A destination called again after it aged out — a monthly job's, in a long month — starts again: a
new `first_seen_at`, `calls` from one. Records are not audit rows
(design/ARCHITECTURE.md, *Audit is not logging*).

**Replaces** `egress_minutes`, `egress_hours`, their daily partitions, the partition and roll-up job
(`maintain`) and the day's-destinations cache. A new migration drops both tables — their partitions
with them — and adds `egress_destinations` (primary key the row's key, an index on `last_seen_at` for
the age) and the gaps' `app` and `env`, `over_cap` in their reason's check, and `app` set exactly when
the reason is `over_cap`, `env` with it. 0079 and 0080 are never edited; the tables hold no data,
egress being inert. Supersedes #2221 (no partition to miss) and #2213 (its test is rewritten on
the new table, its acceptance kept: `-count=5`, `-shuffle=on`, one process).

### 2.5 `via`, the deploy row, patches and rollback

`egress.via` (host setting, admin) is `off | canyon` in v1. **The egress spec a run used** — `via`
— **is recorded on its deploy row**. An app env's mode is not: it is live policy, which egress-control
applies to the running app with no `/run` (§3.2), so a patch never finds one pending (amended 2026-10-19,
#2094). It changes only on a **deploy to
preview** — any, a redeploy of the same package included — which reads the host's setting then: a
promotion, a rollback and a patch's `/run` (design/PATCHING.md) carry a recorded spec — the one the deploy
that started their package in preview recorded, preview being the canary, or for a rollback the one
production's history recorded for that image — or, **where nothing was recorded** (a version from before egress, production's backfilled first row),
the host's current spec — never via off by omission (fail closed, amended 2026-10-11, #2196). So none of
them moves an app off the floor, and only that exception can move one onto it: a version from before
egress, promoted or put back on a host now routed through Canyon, runs through Canyon's Envoy. An
explicitly recorded `off` is carried as `off` onto an open network; the run-config fingerprint covers it.
**The network wins over a spec** (the owner's decision, 2026-10-15, #2254): onto an app whose network is
internal (§2.2) every `/run` carries `canyon` — a recorded `off`, and the host's `off` once via goes off,
included — and its deploy row records `canyon`, what ran; so neither a carried spec nor the host's setting
moves an app off the floor, only an admin's Recreate networks. A rollback (design/OPERATIONS.md), a
promotion of an earlier version, runs that version's recorded spec under **today's**
allowlist — the allowlist is live policy, not part of a version — and its receipt and `canyon
status` list refusals of destinations a person had **dismissed**: those production refuses today, unless the org runs
production (§3.4). A rollback
reads no preview, so `prod.egress-allowed` never holds one (§3.4).

**On Kubernetes the host's `canyon` wins over a recorded `off`** (A′, decided 2026-10-15, #2286).
There is no app network: every `/run` writes the app's NetworkPolicy. So while the host's via is `canyon`
every `/run` carries `canyon` — a promotion of a package preview ran under `off` and a rollback to a version
production ran under `off` included — and its deploy row records `canyon`, what ran: nothing carried moves an
app off the floor while the host routes through Canyon, as on Docker, where the network wins over a recorded
`off`. Under `off` a carried spec runs as recorded. A via flip changes no running app until each env's next
`/run`: preview's next deploy reads the host's via; prod's next promotion carries the spec recorded in
preview, or `canyon` while the host is. A rollout that flips the policy cuts the old pod's calls for its last
seconds (fail closed; #2298). A retry that finds its version already running starts nothing and records no
spec on its row: its first attempt may have crashed before the `/run`, so the pod's policy is not known to be
the floor, and the env reads not under `canyon` until its next `/run` (fail closed). Nothing records an app network on Kubernetes, which would pin an app to
`canyon` with no recreation to undo it: `networks_to_recreate` is `null` there, Recreate networks is `409`
(*an app's NetworkPolicy is written at every deploy: there is no network to recreate*), and `floor` is read
from the deploy rows, per env — `enforced` when every env Canyon runs ran under `canyon`, `not_enforced`
when the host's via is `canyon` and an env ran `off`, else `null`.

## 3. Layer 2 — Restrict

### 3.1 egress-control, a sidecar of Envoy

`canyon-server egress-control` — the server image as a subcommand, like `rotate-kek` — runs in the
**Envoy pod** (Docker: in the same namespace). ext_authz goes over localhost (`CANYON_EGRESS_CONTROL_ADDR`, a
loopback address); Envoy's access logs still go to the server's receiver, which reads the check's metadata on
each entry (#2116: one receiver, its caps and gaps, never a second aggregator); one Deployment,
one NetworkPolicy, no load-balancing of long-lived gRPC, availability one-to-one with Envoy. Two or
more replicas, a PodDisruptionBudget and topology spread; **readiness = holds a policy**, which only a `200`
gives it — it keeps none across a start — and which `canyon-server egress-control probe` tells an exec probe
(#2116); a policy no more than 30 s old, the callers' bound (§3.2), so a replica cut off from the server leaves
the Service while the others serve (#2119). Not inside the one-replica server, whose every restart would otherwise stop every app's outbound calls
at once; beside Envoy, a restart of the server under 30 s stops none, and one past 30 s refuses every call
`unknown_caller` (§3.2, static stability), as a failed check would. ext_authz
runs with `failure_mode_allow: false`, timeout 5 s, `status_on_error: 503` and a body (§8.4): a new pod's first call
waits on the refetch for its caller (§3.2), which gives up at 4 s, so a cold path from the server to the executor has room
(#2119; it was 1 s).

### 3.2 Policy: pulled, authenticated, statically stable

- **Policy, never secrets.** `GET /egress/policy?have=<epoch:seq>` on the server's internal listener
  answers callers (the addresses the server resolved, Docker and Kubernetes alike), modes, allowlists, platform
  destinations — no credential. `304` only on an exact version match; `epoch` changes when the
  database is restored, so a version never goes backwards unnoticed. Secrets are layer 3's, fetched
  per route (§4.1). **The listener** (#2113) is its own, `CANYON_EGRESS_POLICY_ADDR`, never the API's, which a
  load balancer fronts: on Docker the server's address on `canyon-egress-ctl`, port 7782, as the receiver's
  rules (§2.2) — unset, the receiver's address on 7782, which the floor's hole for it defaults to too, so the
  append-only compose file names it nowhere (#2119); on Kubernetes
  `0.0.0.0:7782`, admitted from the gateway's pods alone. **The version:** `epoch` is 128 random bits each
  server start draws and writes, so every server answers one; a running server draws a new one before it
  answers when its read, in the policy's snapshot, finds the database's server started again
  (`pg_postmaster_start_time()`: a restore or a failover always restarts it), or a `seq` lower than one it
  answered under the same epoch — so a version a replica holds is never answered again with other content (`known[]` aside, §3.3, which moves no version), even
  once writes have taken `seq` back past it; `seq` is a row moved by a trigger on every row written to a table the policy is read from — the
  org's via and mode, an app env's mode and allowlist, the front door (its platform destinations), the envs and
  the callers — and on a `TRUNCATE` of any of them, in the writing transaction, so no route can forget it;
  requests, which the policy does not carry, move nothing. The policy is read with its version in one snapshot.
  **The envs** are those Canyon runs under via `canyon`: each named by its deploy step before its `/run`, and by
  its deploy row under via `canyon`, in the row's transaction, so an undeploy racing a deploy never leaves the app
  running unnamed; dropped, its callers with it, when that row goes (an undeploy, a delete, a hold), is handed to
  the org or runs under via `off`, and when the first `/run` that named it fails.
- **Authenticated.** Kubernetes: egress-control presents a projected token (audience
  `canyon-egress-policy`), verified offline against the cluster's service-account key set, over TLS
  pinned to a certificate the server mints and an install step publishes. Docker: the dedicated
  `canyon-egress-ctl` network and `refuseAppNet` on the listener. **Decided** (#2113, 2026-10-18):
  the token rides in `Authorization: Bearer`; it must be signed by the cluster's key set, its issuer the
  cluster's, its audience `canyon-egress-policy`, unexpired, its subject exactly
  `system:serviceaccount:canyon:canyon-egress` and bound to a pod — else `401`, no body. The server reads the
  issuer and the key set from the API server (`/.well-known/openid-configuration`, `/openid/v1/jwks`) with a
  projected token of its own, the default issuer-discovery access and no Role, and trusts a read for **10
  minutes at most**: it reads them again once the last read is that old, or on a key id it does not hold, at
  most once a minute; a read that fails keeps the last one until it is 10 minutes old, and from then every
  request is `401` until a read answers. egress-control's own projected token lives 600 s, so a deleted pod's
  token is refused within ten minutes. The certificate is
  self-signed P-256 for `canyon-server.canyon.svc`, ten years, minted at the first start that serves the
  listener, its key sealed in the vault; `canyon-server egress-policy-cert` prints it, and the install step
  publishes it as the ConfigMap `canyon-egress-policy-cert`, key `tls.crt`, which egress-control pins
  (deploy/k8s/README.md).
- **The replica** is who asks, never what it says it is: on Kubernetes the pod its token names
  (`kubernetes.io.pod.name`), on Docker its address on `canyon-egress-ctl`. egress-control asks **every 5 s**,
  sending the version it holds. Each ask is a row (`egress_replicas`), in the database so a restart or a second
  server sees it: the version the replica holds, when it asked, and `current_at` — the last time its version was
  the current one, or was served it while it was. Settings › Outbound lists each replica asked in the last
  day, its policy's age `now − current_at`.
- **Static stability — 30 s.** A replica serves its last policy **with no expiry** for the envs and allowlists
  it knows; it denies only what it cannot answer (unknown env, unknown caller). **Callers are not static**
  (#2113, the security review): a replica forgets every caller once its policy is more than 30 s old — six of
  its 5 s polls — so a pod address reused while the server is unreachable never carries another app's
  allowlist; the refetch then fails and the call is `403 unknown_caller` (fail closed). So the server down for
  more than 30 s refuses every call out, every app's — the price of that bound, stated (#2116). A replica keeps
  nothing across a start: a policy kept on disk could make no call succeed, its callers past the bound, and
  would only be a path to tamper with; it starts empty, asks with `have` empty, and is ready once a `200`
  answers. Staleness alarms loudly instead: *policy 47 min old* on Settings ›
  Outbound, a metric, a log line per minute. The cost, stated: a narrowed allowlist waits for the
  partition to heal.
- **Unknown caller:** a refetch (`?have=&caller=<address>`), single-flight per address, asked again every 250 ms
  while it names no caller, for 4 s at most, inside Envoy's 5 s (#2119: a new pod calls out before its address
  reaches the cluster's API), which makes the server ask the executor's `POST /callers`; still unknown → `403 unknown_caller`,
  and an address the server's answers named no one for is `unknown_caller` at once for 15 s, with at most 64 refetches running on a replica — one
  refetch per 15 s for an address no one holds (#2119). The server knows no app network's
  /24, so this is how every caller enters the policy, Docker and Kubernetes alike (#2113). **A caller's
  lifetime:** every 15 s the server asks `POST /callers` again for every caller, in one call per 1,024; one the
  executor names otherwise, or not at all, leaves the policy at once, under a new version; one it cannot confirm
  — no answer — is in no answer once it is 30 s unconfirmed, two of the checks: the cutoff is the answer's own
  snapshot's, and each answer that finds such a caller drops it, under a new version, before it answers, however
  long a confirm takes; an env's callers go with the env at once (an undeploy, a delete, a hold, a hand-off, via
  `off`). The bound, exactly: the server names a caller at most 30 s past its last confirmation, and a replica
  holds what it was told at most 30 s more (above), so no caller keeps another app's allowlist more than 30 + 30
  = 60 s past the executor last confirming it, under EKS's 30 s hold of a freed address. **The Docker
  listener has no token:** what admits a caller there is the listener itself, which closes every address of the
  apps' pool, each /24's gateway too, at accept, before a byte is read.
- **Convergence.** The deploy step waits, at most 30 s, until every live replica reports `have ≥`
  the version that names the new env; past it the run fails closed: *the egress gateway has not
  taken this version (<replica>)*. **Decided** (#2113; **amended** 2026-11-12, #2789): a replica is live when it asked within the last 30 s,
  as long as one cut off from the server still serves the policy it holds (its callersKept); `have ≥` is the current
  epoch's and a seq at least the current one once the env is named. The
  wait is inside the step's `/run` under via `canyon`, after it names the env and before the executor is
  called, so past the bound nothing starts; with no live replica — the replicas read, and none live — there is
  none: one that starts later takes the current version before it is ready, and one cut off refuses the env it
  does not know. A live replica holding another epoch's version, or none, is behind; an error reading the
  version or the replicas, or the run's context ending, fails the run before `/run`, never skipping the wait.
  It adds no workflow step and changes no input, so `durableVersion` stays.
  **Every outbound rule change waits too** (#2789, the owner's decision, 2026-11-11; found on the test cluster by #2700,
  where one of two replicas still let a call through after a switch to allowlist had answered): how apps call
  out, the org's mode and limits, an env's mode, an Allow and a Remove, and a route set, moved from a secret or removed. Each
  answers only once every live replica holds the version its committed write made, by the same wait
  (`runs.EgressBehind`), at most 30 s, so what it refuses is refused from its answer on. Past the bound the change
  stands, never rolled back, and its answer, its status and body as ever, carries `Canyon-Egress-Pending: <replica>`,
  the first replica behind, or `unread` when the wait could not be read: what it says fails closed, never what it
  changed. Settings › Outbound, the Outbound card and the needs list say so in place of the change's confirmation.
  With one replica, as on Docker, the wait is that replica's next poll.

### 3.3 Modes, destinations, platform destinations, caps

- **Modes** per app env: `observe` (allowed and recorded) or `allowlist` (only listed; everything
  else refused and recorded). The org default (admin) is either; an owner may only tighten
  (design/PERMISSIONS.md, *Outbound*); no per-app `off`.
- **Canonicalisation** — one function, table-tested, used by Envoy's check, the routes and the
  records: lower-case; one trailing dot dropped; IDNA (UTS 46) to A-labels; userinfo refused; every
  IP-literal encoding refused (dotted, integer, hex, octal, bracketed, v4-mapped); single-label names
  refused; labels ≤63, names ≤253; ports 80 and 443 only. `*.stripe.com` matches any depth below
  and never `stripe.com`. The destination is the `CONNECT` authority, else the absolute-form URL's
  name; an inner request's `:authority` (HTTP/2) or `Host` (HTTP/1.1) must equal it.
- **In every mode** ext_authz refuses IP literals and single-label names, and rate-limits new names
  (30 per app env per minute by default, an admin's cap → `403 rate_limited`, naming the cap), across every
  replica: each lets through the cap over the most replicas that run at once (`CANYON_EGRESS_REPLICAS`), rounded
  down — a cap below the replicas lets no new name through, which egress-control says once (#2691) — which asks nothing of the server, so a
  replica cut off from it still holds the bound. A name is new unless known — on the env's `known[]` in the
  policy (the names a call of it went through to in the last 30 days — never one only refused), allowed by its allowlist, a platform destination, or let
  through by this replica already — so a restart or a rollout starts from what the records hold, never from
  nothing (#2116; contracts/routes.md, *The check*, states the bound).
- **Platform destinations** — the front door's issuer, `jwks_uri`, the endpoints its discovery names
  (token and user-info endpoints), the ALB key endpoint for the region — are computed by the server from
  the front-door settings, allowed for every caller of every env, recorded `by: platform`, never on a
  team's list and never editable there. A front-door host the canonicalisation refuses (an IP address, a
  single label such as an in-cluster `keycloak`) is none: the check refuses a call to it first, and such a
  provider goes direct (§2.2, `CANYON_KUBE_SIGNIN_DIRECT`; #2341), and its discovery is never fetched. The
  discovery's endpoints are recorded by issuer when a choice 2 front door is saved, over the server's own egress,
  so a policy read makes no network call; a save never fails on reaching the provider — it waits at most the fetch's ten seconds — and one whose fetch fails
  saves, says so (`discovery_note`), and the server tries every minute for its whole life until it records them.
  The server reads the discovery of each recorded issuer in use again once a day, so a provider that moves an
  endpoint to a new host is noticed within a day (until then an app under allowlist cannot reach it): the new host
  is added and logged, and the policy's version moves (a read that finds nothing new moves nothing). A read that fails keeps
  every host recorded, is tried every minute until one succeeds, and says so meanwhile (`discovery_note`, D5's
  daily-check line; #2389).
  The platform destinations are every front door in use's — the stored one, the one it replaced for 30 minutes
  (a deploy that read it may still be starting), and each a running deploy was given — so replacing the door
  breaks no app deployed behind the last one before its next deploy; and an issuer's recorded hosts only grow, as a
  sidecar keeps refreshing at the host its document named when it started (contracts/routes.md, the front-door
  row; #2344). **Known limitation**: a provider's discovery hosts are allowed whole, for every app
  — and some are large shared API hosts (`www.googleapis.com`, `graph.microsoft.com`) an app could call for its
  own purposes too; Canyon allows the name, never one path on it.

### 3.4 Refusal, request, go-live

Denied, Envoy answers `403`, `Canyon-Egress: not_allowed`, one-line body (§8.4) — nothing goes
upstream and nothing is resolved. A refusal is **not a SARIF finding** (a finding is a claim about
code; this is a fact about an environment). The first refusal of `(app, env, destination)` files an
**egress request** — `pending`, actor `canyon`, one audit row; at most 50 pending per app env —
which the Outbound card and `needs_you` show: **Allow** or **Dismiss** (a dismissed destination
files no new request). **`prod.egress-allowed`**, *Production allows everything preview connects to*: a new
go-live rule, `producer: server-state`, installed `off`; on, the receipt fails while a destination
preview's row last saw at or after the earlier of the first go-live of the image preview runs and `deployed_at` less
the margin below (its job runs' calls are on the same row; a redeploy's outgoing container counts too — stricter,
never looser) is
not allowed in prod, and fails closed — *could not check* — when a gap since the running deployment went live
names no app or is preview's `over_cap`. Each line, and the order they are judged in, is contracts/routes.md,
*The promote receipt*.

**Rows read from the image, gaps from the deployment** (decided 2026-11-06). `deployed_at` moves on every
deploy, so reading rows from it would let a redeploy of the same image clear a row that holds it, while the same image
still connects to that destination. So rows read from the digest's first go-live, the oldest `deploy.completed` of it
in preview: a new image clears them and the same image doesn't. The margin hangs off the running deployment, never off
that first go-live, which doesn't move: the read starts at the earlier of the first go-live and `deployed_at` less the
margin. V1's last call 5 s before V2 went live counts on V2's first deployment, as nothing tells it from a tunnel V2
holds, and deploying V2 again goes past it, so no one is pushed to allow what the app no longer uses. Gaps and the
records' keep stay on `deployed_at`, as their lines say a redeploy clears them. A first go-live that can't be read is
*could not check*.

**The margin: 90 s** (`egressSeenMargin`; decided 2026-11-06). A live tunnel's `last_seen_at`
lags it: it moves at a periodic entry, every 60 s, and reaches its row at the flush after, within 10 s and that
flush's 20 s of `POST /callers` at most. A tunnel opened at 12:00:03, before go-live at 12:00:20, is next seen
around 12:01:03, so a promotion at 12:00:40 would find nothing since go-live and pass while it is connected. So each
read of a deployment starts the margin early, rows and gaps alike, which counts what the outgoing version connected to
in those 90 s too: stricter, never looser. A test holds the margin to the bootstrap's `access_log_flush_interval`.

**"Allowed in prod"** is The check's answer for `prod` as it stands, by the check's own two steps (`Canonical`, then
`Refusal`): IP addresses, `invalid` and ports other than 80 and 443 are refused in every mode (§3.3), so `observe`
allows none of them; then a platform destination; then `observe`; then an allowlist entry. A row's last outcome is
not read: preview may have been refused what prod allows, or rate-limited. `prod`'s deploy row is not read either: a
promotion carries preview's spec (§2.5), so prod's mode and allowlist are what the version meets.

**The org's platform on both sides: neutral** (the owner's decision, 2026-11-07, #2125). Canyon doesn't control the
outbound connections of an app the org's platform runs, so a preview the org runs doesn't block. That holds only while
production will be the org's too, so the row goes neutral only when two reads both say `org`: `run_by` on preview's
deploy row and the org's `run_by` setting. The setting decides where a promotion goes (design/HAND-OFF.md §1), and
preview's row alone doesn't. The row then reads `not checked: preview runs on your own platform`, `unset` as an unset
`stack.blessed` is, whatever the rule's state and never in `blocked_by`. After a switch back to `canyon` preview's row stays `org` while the promotion starts the image on
Canyon, behind production's allowlist, with nothing read of what it connects to: that row is *could not check*, and a
deploy to preview clears it. A `run_by` or a setting that reads as neither value, or doesn't read, is *could not check*
too, never neutral. **A known limit:** the receipt reads the setting and the run reads it again as it starts, so a
switch between the two isn't seen. It takes an admin, in the moment a promotion starts.

**What it cannot read never passes.** A preview not through Canyon (no `canyon` spec on its row;
on Docker a `floor` not `enforced`, whose calls may go around Envoy) is *could not check*: no row is no evidence. Its
line gives the fix the app's Outbound card gives: an admin sends apps through Canyon where the host's via is `off`, or
recreates the app's network where it is on the old one, since a deploy to preview goes around Canyon again in both
(#2723). So
is a preview recreated onto Canyon after it went live (`run_config_at` past `deployed_at`), the path every Docker app
takes when it first turns on egress: what it reached before is on no row until it deploys again. So is a window older
than the 30 days records keep (§2.4), an aged-out row being an unread one, and a receiver that is down now, its
heartbeat more than a flush and a half from the read either way, whose `receiver_down` gap is written only when it is
back; the server's clock stamps that heartbeat and the database's reads it, so a small skew reads as up.

**The costs, stated:**

- **No soak time.** The rule reads what preview has done, never what it will do, so promoting seconds after a fresh
  deploy passes with whatever it has connected to by then.
- A call in the flush not yet written is not read, as no call after the promotion is.
- A long plain `http://` call in flight, and a `GET` with `Upgrade: CONNECT`, have no row until they end (§2.4): only a
  `CONNECT` tunnel is logged as it opens.
- On Kubernetes, where `/run` answers once a pod is ready, a call an app makes only as it starts, more than 90 s
  before that, is before the window. And an old pod still connecting after go-live lands past the first go-live, on
  the new image's rows: deploying the same image again doesn't clear it, a new image does.
- **A, then B, then A again:** A reads from its own first go-live, so it reads what only B connected to, until a new
  image clears it. The read stays this wide, as anything narrower would let B then A wash a row.
- A row unseen for 30 days is gone (§2.4), however long the image has run.

**Gaps** are decided in one place, `egressGapHolds`. Every gateway rollout writes one that names no app (§2.4, *A
healthy stop*), which by this rule makes every app's row read *could not check* until its preview next goes live;
whether such a gap should only add a note, while everything specific to the app still blocks, is the owner's
decision to make (#2125).

**A rollback** reads no preview, and what production ran before is no question this rule asks: its row never blocks,
so a person putting a version back is never held by what an unrelated version in preview connects to. It passes, or
lists the destinations a person dismissed that production refuses today (§2.5), an advisory line that stops nothing,
which the dashboard and the CLI say before the person promotes. On a production whose deploy row says `org` it lists
none and passes: the Outbound card has no Allow there, so the line would send a person nowhere. A row that doesn't
read leaves the line, which stops nothing. An app with nothing in preview has no row on its
receipt, and Guardrails' preview names it nowhere (contracts/routes.md, the policies row).

### 3.5 Who changes what

| Who | Read | Allow · add or change a route | Remove · dismiss | Mode | Host settings |
|---|---|---|---|---|---|
| **Admin** | every app | yes, dashboard only | yes | either way | yes, dashboard only |
| **Owner** | apps they own | yes, dashboard only | yes | stricter than the org | read |
| **Contributor** | apps they contribute to | yes, dashboard only | yes | read | read |
| **Read-only, none** | nothing | no | no | no | read |

Widening — allowing a destination, adding or changing a route, `from-secret`, an app's mode back
to the org's — answers `403 {error:"dashboard_required", message:"change it in Canyon's
dashboard"}` to a non-browser session (contracts/routes.md, the preferences row's body, byte for
byte) and `403 person_required` to an `agent` or `canyon` actor; no CLI verb exists. Narrowing an
app's settings works from any session; dismissing is a person's; every host-settings change is an
admin's, in a browser. The rest — a secret's route, a held production, the order of refusals,
what a member reads — is design/PERMISSIONS.md, *Outbound*.

### 3.6 DNS

**Docker:** closed — an internal network has no route out, and Docker's resolver answers no public name there;
`TestEnvoyCallOfAnAppRoutedViaCanyonIsItsRow`.
**Kubernetes: DNS stays open until its own later PR**, and the docs say so: kube-dns forwards any
name, so an app can tunnel data through lookups even under an empty allowlist. That PR uses
`hostAliases` plus an unreachable nameserver where the database name is a stable ClusterIP, and a
stock CoreDNS (`CANYON_IMAGE_EGRESS_DNS`) only for external database names.

## 4. Layer 3 — Inject (the compose spike's findings, §13)

### 4.1 Value routes

A route is `(app, env, destination)` → `{header, scheme: bearer|basic|raw, value, placeholder_env?}`;
the destination must be allowed. **The value belongs to the route**: entered with it, sealed under
the route's own vault row, so a write-only secret cannot be re-pointed at another destination;
`from-secret` (owner only) moves a runtime secret in and audits the destination. egress-control
fetches a route's value from the server on first use (authenticated as §3.2), caches it, and drops
it when the policy names a new `rev` for the route, the route goes or the epoch changes; it holds it in memory alone,
and no log line, error or refusal carries it. The policy names an env's routes only while the env
trusts the root signing the current leaves (§4.4, §4.5). `placeholder_env` gets `canyon-adds-the-real-key` so
SDKs start; **a placeholder carries no authority** — the route is chosen by caller and destination,
and the header is overwritten whatever the app sent. **OAuth grants come later**: then `token_url`
is fetched *through the door*, https only, and must be an allowed destination of that app env.

**An inner request goes on only to its chain's destination, bound twice** (#2140; asked by the security review
of #2139): the chain's routes send on only an authority of exactly the destination or the destination and `:443`
(Envoy lower-cases the authority to match it, HTTP/1.1's `Host` and HTTP/2's `:authority` alike), and answer any
other `421 misdirected` themselves, a direct response with ext_authz disabled, so it rests on nothing the check
does; and ext_authz #2 compares the authority with the destination canonically — lower-case, a `:443` dropped, a
byte past ASCII never equal — and refuses any other `421` on its own. Either alone refuses another name, another port
and every address, egress-control's own `127.0.0.1:7783`, where the leaves' keys are served, among them. **The
trailing dot:** the chain's connection manager drops one trailing dot of the authority (`strip_trailing_host_dot`)
before its routes and its filters see it, and ext_authz #2 drops none of its own, so `api.stripe.com.` is
`api.stripe.com` to both, a second dot Envoy leaves (`api.stripe.com..`) is another name to both, and the two never
disagree (#2140's review). **An inner request's caller expires with the
policy** as an outer call's does (decided 2026-11-08, #2140): once the policy egress-control holds is more
than 30 s old, ext_authz #2 refuses every request inside every tunnel `unknown_caller`, so no open tunnel outlives the
static-stability bound (§3.2) — fail closed, as the outer check. **The inner hop re-checks and is bound to the outer
approval** (decided 2026-11-08, #2140, from its security review): ext_authz #2 makes ext_authz #1's decision again of
the chain's destination, by the same function — a route outlives the allowlist entry that let its destination
through, so a tunnel opened before an entry went, or a mode made stricter, refuses its next request for the outer
check's reason — and refuses `misdirected` a request whose tunnel ext_authz #1 approved for another destination, the
approval riding in filter state as the caller does (`canyon.destination`, §4.2), so a tunnel approved for one routed
destination never carries a request, and its credential, to another.

### 4.2 Interception — inject destinations only

ext_authz #1 sets `x-canyon-intercept` only for a destination with a route, after an RBAC filter —
the first filter on :3128 — has rejected every request that already carried it, before any check.
The RBAC filter is not optional: without it the spike's forged header reached any cluster by name,
the ext_authz gRPC port or another env's intercept path with that env's credential. ext_authz #1 also
lists `x-canyon-intercept` in `headers_to_remove` on every OK that does not set it, as defence in
depth; it runs with `clear_route_cache: true`. Envoy 1.39.3 clears the route cache on a remove-only
response although `ext_authz.proto` says only an added or altered header does, so
`TestRemoveOnlyResponseReroutes` pins that behaviour instead of trusting it.

**One internal listener and one internal cluster for every env.** The intercept route matches
`connect_matcher` *and* the header — so a header set on an absolute-form request never routes
plaintext into a TLS listener — and names the one internal cluster; `cluster_header` is not needed.
The caller rides across in filter state, not in the listener's name: ext_authz #1's OK carries the
caller as dynamic metadata, `set_filter_state` copies it to `canyon.caller` with
`shared_with_upstream: ONCE`, and on the internal listener `header_mutation` overwrites
`x-canyon-caller` from it (the CheckRequest has no filter-state field, so a header bridges it; an
app's own `x-canyon-caller` is overwritten). The destination ext_authz #1 approved the tunnel for rides the same
way (#2140): its OK's metadata `destination`, the filter-state object `canyon.destination`, and `x-canyon-destination`
on the internal listener, which ext_authz #2 refuses `misdirected` when it is not the chain's destination, so a tunnel
approved for one destination never carries a request for another. ext_authz #2 refuses an empty caller — what an inner
hop reached without ext_authz #1 carries, as each `header_mutation` keeps an empty value (`keep_empty_value: true`) so it
still overwrites the app's own header (#2140's review; this said `-`) — then removes the header, so the upstream never
sees it. Each
`CONNECT` tunnel is its own internal connection, so one caller's filter state never rides on
another's. `context_extensions` carry only `{destination}`, per filter chain.

**xDS is LDS and SDS.** The listener has one filter chain per inject destination, its leaf by SDS; a
new inject destination is a new chain, which SDS alone cannot add — so layer 3 serves **LDS and SDS**
from egress-control (`go-control-plane` and `grpc`, Apache-2.0, named in their PR), built
deterministically from the policy version so replicas never disagree; clusters and routes stay
static, and a new env needs no xDS push. Envoy keeps its last listeners and certificates when xDS is
gone; every call is still checked. A chain is served only for a destination whose leaf egress-control holds under the
signing root and inside its validity — asked for again from half its life, the one held kept while the server cannot
answer — and no listener without a chain: an intercepted call finds no chain and its connection closes, or no
listener and is `503`, never forwarded without its check. A leaf is dropped per destination, the others' chains kept
(decided 2026-11-08, #2139): the listener has no default chain and no chain matching another name, and
ext_authz #1 decides interception by the policy's routes, never by the listener served, so a destination whose chain
was dropped still goes to the internal listener and fails there, never to the plain tunnel. Upstream TLS uses `max_session_keys: 0`, so a session for one
destination is never resumed for another on a shared CDN address. Tunnels end at
`max_stream_duration`; a stopped app's tunnels end with its pod.

**The receiver's token** (decided 2026-11-08, #2740). Each chain logs its inner requests to the
receiver as `:3128` does, so egress-control holds `CANYON_EGRESS_RECEIVER_TOKEN`, from its environment — on
Kubernetes the Secret `canyon-egress`'s `token`, as the server and Envoy read it — and serves it Envoy in the LDS
answer alone, over loopback: never in a log line, an error, a metric or an SDS answer. Without a valid one,
egress-control serves no interception listener and asks for no leaf: a call to a destination with a route finds
none and is `503`, so no inner request goes unrecorded; it says so once as it starts, and checks every other call as
ever, so a manifest that missed the token takes no app's calls down.

**`upstream_failed` is only a route's** (#2141): on `:3128` only a request that is no `CONNECT` and whose check's OK
carried `destination` — the absolute-form inject branch's — gets it, by a `metadata_filter` with
`match_if_key_not_found: false` (Envoy's default matches a call without the key, which answered the floor's refusal of
a plain `http://` call `502 upstream_failed` on the stack), so a plain tunnel that fails never reads a line about
credentials; on the internal listener every request does. The chains' cluster is a
dynamic forward proxy's sub-clusters, one per destination, and a sub-cluster answers a name that does not resolve
*no healthy upstream* (`UH`, seen on the pinned Envoy), not `UF` or `DF`, so the chain's mapper counts `UH` too.

**Absolute-form `https://` to an inject destination.** A client that sends `GET https://…` instead
of `CONNECT` (axios ≤ 1.7) hands Envoy the request in clear; Envoy originates the TLS. ext_authz #1
has a second inject branch for it: on such a request to an inject destination it overwrites the
header itself and applies ⓓ's checks — value unreadable `502 inject_unavailable`, plain `http://`
`403 inject_requires_tls` — with no interception and no internal listener. Envoy takes `:authority`
from the URL and drops `Host`, so the checked name is the dialled name and the SNI. **This is within
*decrypt only inject destinations*:** that rule limits which TLS Envoy terminates, and here Envoy
terminates none — it sees what it sees of every absolute-form request since layer 1, and the
injected credential leaves only inside TLS Envoy originates and verifies. When ext_authz #1 cannot
tell whether the destination has a route, the request is refused, never forwarded without the header.

### 4.3 The caller from layer 3 — the pod's own token

Injecting a secret must not rest on an address that could change hands, so from layer 3 a
Kubernetes caller proves itself: the executor gives each app env a ServiceAccount
`canyon-app-<app>-<target>`, mounts a projected token (audience `canyon-egress`, 10 min, pod-bound)
into `canyon-identity` alone, and points the app's proxy variables at its `127.0.0.1:4184`
forwarder, which adds `Proxy-Authorization` to each `CONNECT`. ext_authz verifies the token offline
against the cluster's key set and removes the header. Docker keeps its address plan: per-app
networks, fixed slots, no raw sockets.

How it lands (**added 2026-11-09**, #2142; contracts/routes.md, *The check*; contracts/executor.md, The pod's
token):

- **The key set travels in the policy.** The server reads the cluster's key set as it does for
  egress-control's own token, and the policy carries it, so egress-control needs no access to the API and
  keeps its static stability. With no key set read, no token verifies.
- **A valid token always names its env**, from any address. A token caller is never trusted longer than an
  address caller: 30 s of policy, 10 minutes of key set.
- **The mark governs the address fallback alone.** Pods deployed before layer 3 have no forwarder. Their env
  stays its pods' address's, but a call named so is never intercepted. Once a `/run` whose answer says its
  pods carry the token (`token_callers`) succeeds with its earlier pods gone (`previous_gone`), the server
  marks the env `token_only`, from then named by its token alone (no downgrade). The mark is the server's:
  no pod and no executor's `POST /callers` sets it, and an older executor's answer, which carries no
  `token_callers`, never does.
- **Job pods are deferred** (#2142's review). No job runs on Kubernetes yet. When jobs come, a job pod
  is named by its address and never intercepted until its forwarder-only native `canyon-identity`, the
  table's last row, exists.

| Caller | Layer 1 | Layer 2 | Layer 3 |
|---|---|---|---|
| Docker web | slot `.10`–`.13` | same | same |
| Docker job run | slot `.14`/`.15` | same | same |
| Kubernetes web | pod IP via `POST /callers` | same + refetch | projected token through `canyon-identity`'s forwarder |
| Kubernetes job run | pod IP via `POST /callers` | same + refetch | token through a forwarder-only native `canyon-identity` |

### 4.4 Proxy variables, CA trust, honest client support

The executor owns, beside `PORT` and `CANYON_IDENTITY_*`: `HTTPS_PROXY`/`HTTP_PROXY` (and
lower-case), `NO_PROXY` (§2.2), `NODE_USE_ENV_PROXY=1`, and appends its proxy properties to
`JDK_JAVA_OPTIONS` (never owning `JAVA_TOOL_OPTIONS`, where apps put `-Xmx`). From layer 3 it mounts
the interception root at `/etc/canyon-egress/` — the root under its OpenSSL subject-hash name
(`<8 hex>.0`, the only names Paketo's JVM certificate loader reads) plus `bundle.pem` — and sets
`SSL_CERT_DIR=/etc/ssl/certs:/etc/canyon-egress`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`. A JVM
takes the root at launch through that loader, which reads `SSL_CERT_DIR`, so it needs no trust property.
`REQUESTS_CA_BUNDLE` names a file of the system roots and the root, as Python's `requests` reads it in place of its
own. When an env's own secrets set one of the three, the executor sets none of them and its `/run` carries no root.
Only Canyon's runtime env is read for them: one an image sets in its own launch env is overridden by the executor's,
which Canyon's builder makes moot today, as it takes no bindings that would set one (#2143's review).
Each deploy row records the roots every instance that may be serving trusts; Canyon intercepts an env's routes only
while the root signing the current leaves is among them (contracts/routes.md, *Interception*, Trust), and until then
a route's destination passes through untouched.
**Honest support:** axios ≥ 1.19 sends `CONNECT` (no User-Agent) and so needs `NODE_EXTRA_CA_CERTS`
for an inject destination; axios ≤ 1.7 sends absolute-form `https://` and is injected without
interception (§4.2). A client that sends `CONNECT` over HTTP/1.0 (`openssl s_client -proxy`) gets
`426`. `NODE_USE_ENV_PROXY` covers only Node's global agent (Node 22.21+/24.5+; the AWS
SDK v3 handler bypasses it); Apache HttpClient 5 and Reactor Netty ignore the JVM's proxy
properties; grpc-go `NewClient` 1.63–1.69 resolves locally. A client that ignores the proxy times
out — so `canyon status` and the shipped skill say *a call that times out to an outside service is
a client that ignores HTTPS_PROXY*. Fixtures prove each claim (§11).

### 4.5 Keys

New vault-sealed tables, each in `main.go`'s two boot-invariant checks and `rotate.go`'s
`rewrapOne` list and remaining count, with `rotate_test.go`'s `sealedRow` (AGENTS.md): the
interception root (5 years) and route values; an admin's sub-CA's key is sealed as a root's is, in
the roots' table. The server keeps a 7-day intermediate with **nameConstraints = the union of
inject destinations** (and every IP address excluded) in its memory alone, its key never written
anywhere, re-minted on any route change, a change of the signing root and a restart, and mints
one-SAN 24-hour leaves for SDS, which egress-control fetches (`GET /egress/leaf`). **BYO:** an
admin may supply a corporate sub-CA instead of Canyon's root, so apps already trust it. Canyon's
intermediate under it is name-constrained the same way, and the sub-CA must carry name constraints
of its own that permit every destination a route names, none of them empty, and allow no IP address
but ranges it names: a CA key with none in Canyon's hands would
let a compromised Canyon sign for every name the org's devices trust (decided 2026-11-08, #2704).
**Rotation** (decided 2026-11-08, #2704): Canyon starts one itself 60 days before its own newest
root's end, and an admin starts one at any time, the same route taking a sub-CA
(`POST /api/v1/egress/ca/rotate`); one at a time. Rotation keeps both roots in the bundle for 60
days, the old one ending at its certificate's end or 60 days after the rotation began, the sooner;
each is an audit row, `egress.ca.rotate`. A sub-CA never rotates by itself: Settings › Outbound
shows its end ahead, and past it, with no other root, nothing is intercepted. Leaves are signed
under one root at a time, the signing root: on rotation or a switch to a BYO sub-CA the old root
signs on until no env holding a route lacks the new one — an env counting only while Canyon runs
it, so one with no deploy row holds nothing back and its routes wait for its first deploy — and an
env still lacking it at the old root's end waits; a root passing its end moves the policy's
version (contracts/routes.md, *Interception*, Trust).

## 5. Later layers — the roadmap

| Mode (`via`) | See | Restrict | Inject | Gate before building |
|---|---|---|---|---|
| **E1** Canyon-run Envoy (`canyon`) — layers 1–3 | ✓ | ✓ | ✓ | — |
| **E2** Canyon-run Envoy chained to the org's proxy (Zscaler, Netskope, Squid, cloud firewall) | ✓ | ✓, stricter wins | ✓ — the org's proxy sees the injected credential if it inspects TLS | spike: Envoy DNS blackholed; trigger: a customer whose egress is an explicit proxy |
| **E3** the org's Istio, Canyon as ext_authz (`org-envoy`) | ✓ only what the mesh routes to the gateway | ✓ by SNI, no decryption | ✗ — use E2 through their gateway | the Istio kind spike; trigger: a customer on Istio |
| **E4** the org's egress only (`org`) | ✗ | ✗ — apps reach only the org's proxy | ✗ | trigger: a customer who declines E1 |

- **E2.** A pass-through `CONNECT` is forwarded **unterminated** to a static cluster, the org's proxy
  (Envoy resolves nothing; the name travels); ext_authz can set the org proxy's `Proxy-Authorization`
  from the vault. `egress.ca` is Canyon's root **plus** the admin's org CAs — in every mode, since a
  transparent Zscaler tunnel also needs only the CA.
- **E3.** The org runs a **dedicated Canyon egress gateway** (`istio: canyon-egressgateway`), its
  `AuthorizationPolicy` `action: CUSTOM` with **no `from`** — so every request asks Canyon, and a
  request without a mesh principal is `unknown_caller`; a `DestinationRule` with `ISTIO_MUTUAL` and
  `tls.sni: <destination>` per destination, so the check sees the real name; `envoyTcpAls` for
  pass-through bytes. `canyon-apps` stays PERMISSIVE for the unmeshed router on 4180. Saving
  `org-envoy` requires a **canary**: a canary pod's check arrives with a mapped principal and a
  canary destination is refused. Fail-closed is the org's `failOpen`, which Canyon states and watches
  (*no checks from your gateway since 10:02*).
- **E4.** `HTTPS_PROXY` = the org's proxy; the floor admits only its CIDRs; modes and routes
  `409 egress_external`. **Hand-off** (`run_by: org`): every layer is the org's, `egress_external`.
- **Layer 4 — identity outward.** The SPIFFE ID `spiffe://<canyon trust domain>/canyon/<app>/<env>`
  stays the contract; JWT-SVIDs per call signed in the server, `/.well-known/spiffe-bundle`, a
  `workload_token` route whose audience is bound to its destination (admin when they differ), the
  org's SPIRE as a second proof of the caller, mesh X.509-SVIDs mapped. Canyon's trust domain is
  always its own; a `PUT` naming a federated domain is refused. Trigger: the first internal API that
  authorizes callers by workload identity.

## 6. Fail closed — every path

| Layer | When | What happens | Never |
|---|---|---|---|
| 1+ | Envoy down | the app's calls fail to connect; 2+ replicas, PDB | a direct route out |
| 1+ | an allowed name resolves privately, or `CONNECT 169.254.169.254:80` / `canyon-server:80` | refused by Envoy's own floor | a tunnel inward |
| 1+ | a port other than 80/443 | `403 port` (layer 1: no route) | — |
| 1+ | `CANYON_KUBE_SIGNIN_DIRECT` holds an empty entry, a host out of shape or twice, or an address without `/32`, a range, IPv6 or link-local | the Kubernetes executor does not start | a wider hole in the floor |
| 1+ | a listed sign-in host's URL names a port other than 443 | the `/run` is `400` | a sidecar left calling a port the rule never admits |
| 1+ | records cannot be delivered | calls continue; gaps recorded and shown; `prod.egress-allowed` cannot pass over a gap | silence |
| 2+ | Envoy's stats cannot be read | calls continue; an `envoy_dropped` gap no one counted, extended at every read, and a log line a minute (§2.4) | reading as none dropped |
| 2+ | ext_authz does not answer in 5 s | `503`, body *Canyon's egress check did not answer* | allowing |
| 2+ | the server is down | up to 30 s, known envs and callers keep their last policy; past it every call `403 unknown_caller` (callers expire), an address's first after 4 s of refetch, then at once for 15 s (#2119); on Kubernetes a replica leaves the Service then, its readiness a policy at most 30 s old, and with every replica cut off a call fails to connect; staleness alarms | a caller kept past its bound |
| 2+ | a policy fails its schema's known fields, names an address or env twice, has a higher `format`, or its epoch went backwards | refused whole; the previous one serves | a partial or older policy |
| 2+ | the mode is not one of the two | `403 not_allowed` | `observe` |
| 2+ | IP literal, single label, over-long, rate-limited name | `403 invalid_destination` / `rate_limited` | — |
| 3 | xDS gone | last listeners and certificates; every call still checked; a missing listener is `503` | pass-through for an inject destination |
| 3 | egress-control holds no valid receiver token | no interception listener: a call to a destination with a route is `503`; every other call checked as ever | an inner request unrecorded |
| 3 | a request already carries `x-canyon-intercept`; an inner hop carries no caller (empty) | `403` by RBAC, before ext_authz · `403 unknown_caller` | reaching a cluster by a header the app chose |
| 3 | leaf expired, value unreadable, plain `http://` to an inject destination, wrong `:authority` | handshake fails · `502 inject_unavailable` · `403 inject_requires_tls` · `421` | forwarding without, or in clear |
| 3 | upstream connection, certificate or name fails for a route's destination | `502 upstream_failed` (§4.2) | skipping verification |

Records are the one place calls continue: evidence, not a gate.

## 7. Operate it

- **Envoy admin** only on its Unix socket: an ephemeral container mounting the pod's `admin` volume, `curl
  --unix-socket /var/run/envoy-admin/admin.sock http://x/stats` (Envoy's image has no curl). **Metrics:** Envoy's and egress-control's Prometheus
  endpoints on a port only the cluster's scraper is admitted to, or OTLP export; the access-log sink
  (records) is readable by the org's SIEM through `GET /api/v1/egress/destinations` and a daily
  export.
- **One log line per denial** from egress-control: app, env, destination, reason, Envoy's stream
  id — never a path, and never the CheckRequest itself (§2.4).
- **The 3am runbook:** `canyon status` → the Outbound row's flag → egress-control's log by stream id
  → Envoy stats over the socket. *No record at all* means the client bypassed the proxy or records
  cannot be delivered; `503` with Canyon's body means the check did not answer.
- **Images:** `CANYON_IMAGE_EGRESS`, `CANYON_IMAGE_EGRESS_DNS` and, on Docker, the namespace
  holder's `CANYON_IMAGE_EGRESS_HOLDER` override the pins for mirrors
  and FIPS builds. Pins follow the two-week age rule **except Envoy security releases**, bumped at
  once. Supported: the pinned Envoy and the two minors before it; E3: Istio N-2.

## 8. The experience

### 8.1 `canyon status`

```
  outbound     preview: only allowed destinations · 2 destinations refused since v1.4 went live
                 ✗ api.openai.com    not allowed 14 times, last 2m ago
                 ✗ hooks.slack.com   not allowed once, 1h ago (dismissed)
               production: recorded · 6 destinations in the last 7 days
```

`only allowed destinations` is an env under allowlist, `recorded` one under observe. The destinations are rows last
seen in the window; a refusal count is its egress request's (layer 2), as a row keeps only the last result, and an
env lists the requests last refused since its version went live, the tail reading `2 destinations refused since v1.4 went live`
then, else `6 destinations in the last 7 days`. Other values: `not yet through Canyon: deploy again` · `may skip Canyon until an admin recreates its
network in Settings › Outbound` · `runs on your own platform` · on Kubernetes, which has no network to recreate, `may skip Canyon until its next
deploy or promotion` (#2286), the floor each env's own pod's there: under `not_enforced` for an env
not run under `canyon`, one run under it recorded; for every env, none recorded, under a `floor` the reader does
not know or none under via `canyon` (fail closed). Below contributor there is no `outbound` row.

**After an Allow** (#2722): an Allow closes the env's requests, so the destination leaves the refused lines. In run 3
of the #2162 journey nothing else said it was allowed, and the agent read the app's old failures as current. So an env
through Canyon names, after its refused lines, each destination a person allowed since its version went live, with who
and when, where that Allow closed a refusal, the env is under allowlist and it runs (only a refusal proves the client
uses the gateway): `✓ api.openai.com   allowed by Dana Whitfield 6m ago · calls go through now, no deploy needed`. The read's
`allows[]` records each Allow (contracts/routes.md); an entry allowed before it records no one and is never named.

### 8.2 `--format ai`

```
outbound: preview · only allowed destinations · 2 destinations refused since v1.4 went live
refused: api.openai.com · 14 calls · last 2026-10-11T10:02:11Z
outcome: Canyon refused these calls with HTTP 403. They never reached the destination.
Only a person on the app's team can allow a destination, in Canyon's dashboard. You cannot allow one.
do:
  1. Tell the user which destinations were refused and why the app needs each one.
  2. Give them this link: https://canyon.meridian.example/#/apps/claims-portal/outbound
     Once a person allows a destination there, the app's next call to it goes through within seconds, with no deploy.
  3. If a destination is not needed, remove the call from the code and deploy again.
do not:
  - reach the destination another way: an IP address, another port, a DNS lookup, a raw socket
  - unset or change HTTPS_PROXY, HTTP_PROXY or NO_PROXY
  A person on the app's team decides what the app reaches; any other way goes around them.
```

One `outbound:` line an env the read names, its mode beside what it refused since its version went live (`no
destination refused`, `1 destination refused`), and a dismissed one's `refused:` line ends ` · dismissed`; an env
not through Canyon reads its §8.1 value. The block from `outcome:` on prints once, while any env refused a destination.
An Allow reaches the running app with its next policy (§3.2), so the link's step says no deploy is needed, and the line
under `do not:` says why no other way is the agent's to try, so it has a true reason to give rather than one it makes
up (#2708: in the first agent journey, the agent said Canyon flags and audits such calls). It names who decides, not
the floor: on Kubernetes the cluster's DNS resolves any name, and database ranges and sign-in hosts stay reachable
(§2.2), so "nothing but the gateway" would not be true there. With nothing refused, an env through Canyon and
`prod.egress-allowed` not blocking, an agent reads the one line `If a call to an outside service times out, or can't
resolve the host name, the client may be ignoring HTTPS_PROXY.` in the block's place (§4.4): a refused call was a
`403`, never a timeout. `canyon logs` names the destinations refused since the version went live after the app's
lines, where status lists them (design/specs/cli.md §6.4.1), as a client may log a refused call only as a failure.
While `prod.egress-allowed` blocks with nothing refused, as preview under observe and production under allowlist leave
it, an agent gets the same `Only a person…`, the link and `do not:`, under `outcome: prod.egress-allowed stops this
version reaching production.`, and a refused promotion's block carries them too. A row that could not check names no
destination, so none of that is true of it: in their place, in both, an agent reads the one line `outcome: Canyon could
not check outbound connections, so this version can't reach production. Do what the prod.egress-allowed line says. An
approval won't clear this.` Layer 3 adds
`put a credential for an injected destination in the code or in a secret: Canyon adds it` to `do not:`.

After an Allow (#2722), each destination §8.1 names is one line under the env's, `allowed: api.openai.com · by Dana Whitfield at 2026-11-08T11:37:50Z · calls go through now, no deploy needed`.
Then a block prints once, after the refused or held block when one prints, and in place of the `HTTPS_PROXY` line,
since an allowed call reaches the gateway. It names each env's Allows on a line of their own, as another env may still
refuse the same name, says that env's calls go through now, tells the agent to check the app's next
call before it changes anything, and lists `change the code or deploy again for a destination a person allowed in that environment` under `do
not:` (design/specs/cli.md §6.1.5). `canyon logs` names the same destinations, with who allowed each, when, and when
Canyon last refused it, so an agent can place the app's failures before the Allow (design/specs/cli.md §6.4.2).

### 8.3 Dashboard

**App page › Outbound** (contributor+): an env not through Canyon says its §8.1 value, in the same words; per env the mode, refused destinations first: each request, pending
ones with Allow and Dismiss, then dismissed ones with Allow, marked so; under Production, what preview connected
to that production's allowlist doesn't hold, each with Allow, so a person can answer the receipt's line; then
destinations called in the last 30 days, the latest seen first — last seen, calls, last
result (`2xx` · `tunnel failed: UF` · `refused` · `no answer`, a call whose class is `none`; `refused: an IP address` and `refused: port <port>` where no allowlist can let it through, #2723; Envoy's `DC` and `UC`, a side closing, are how a tunnel ends, and any other flag says it failed) — the env's `over_cap` gap as *N calls to destinations
past 500, not listed* (*1 call* for one), platform destinations apart, routes (never a value). **Settings › Outbound** (every member reads `via` and the org default
mode; an admin changes them and reads the rest, #2422): `via`, org default mode, networks to
recreate, resolvers, each replica's policy age, the fleet's destinations, key dates. **Fleet:**
`needs_you` counts pending requests the env refuses now, and so does the `refused` tile, whose line in *Needs you* names
each app with the way to its Outbound card (#2723): the mode refusing the name, the running deployment through Canyon,
and refused since the run that started it began, less the promote receipt's margin, one predicate the app's egress
read says as each request's `refusing`. **App page:** the
headline never says a version is live in green while production refuses such a request; it says what production can't
reach, with the way to the card's Allow. **Guardrails:** `prod.egress-allowed`.

### 8.4 Proposed strings (for D5 once accepted)

- `Canyon refused this call: <destination> is not on <app> <env>'s outbound allowlist. A person on the app's team can allow it in Canyon: <url>`
- `Canyon's egress check did not answer, so this call was not made.` (the `503` body)
- `Canyon refused this call: Canyon does not know which app is calling.`
- Rule `Production allows everything preview connects to`, in D2; receipt line `production does not allow <destinations>`,
  in D5 with the row's other lines (contracts/routes.md, *The promote receipt*).

## 9. Contracts touched

- **Seam 4, executor:** `/run` and `/job` gain `egress {proxy, no_proxy[], ca?}` and `/run` gains
  `run`; Docker `--internal`, `--ip-range`, the fixed slots, `canyon.gate_run`; `POST /callers`;
  a network-recreation call; Kubernetes `networkPolicy` egress and Envoy's peer, proxy variables on
  all three containers; layer 3: ServiceAccount, projected token, `:4184`, `canyon-egress-ca`.
- **Seam 3, routes:** the rows below; an *Egress control* group naming what Canyon reads from and
  answers in ext_authz and ALS (`x-canyon-intercept`, `x-canyon-caller`, the filter-state object
  `canyon.caller`, `context_extensions` keys, dynamic metadata in namespace
  `envoy.filters.http.ext_authz`, denial bodies); `egress.schema.json` (destinations, gaps, policy,
  routes). **Seam 5:** `prod.egress-allowed`, its own PR. **Seams 1 and 2:** none.

| Route | Minimum | Refusals |
|---|---|---|
| `GET /api/v1/egress` · `PUT` | GET any member (`via`, the org default mode; the rest admin); PUT admin, browser | `admin_required` · `dashboard_required` · `409 egress_not_ready` (no `CANYON_EGRESS_PROXY`); no probe of its own: the front door's gates every Kubernetes `/run` (§2.2, #2286) |
| `GET /api/v1/egress/destinations` | admin | `admin_required` |
| `GET /api/v1/projects/:id/egress` | contributor | `team_required` |
| `PUT …/egress/:env/mode` | owner (stricter) · admin; back to the org's or looser: person + browser | `owner_required` · `person_required` · `dashboard_required` · `409 looser_than_org` · `409 floor_not_enforced` · `409 egress_external` · `409 held` |
| `PUT …/egress/:env/destinations/:destination` · `DELETE` | contributor; PUT person + browser | `team_required` · `person_required` · `dashboard_required` · `400 invalid_destination` · `409 held` |
| `POST …/egress/:env/requests/:destination/dismiss` | contributor, person | `team_required` · `person_required` · `404` |
| `PUT …/egress/:env/routes/:destination` · `DELETE` · `POST …/from-secret` | contributor (from-secret, and a route that came from it: owner); PUT and from-secret: person + browser | as above · `owner_required` · `409 destination_not_allowed` · `400 invalid_route` |
| `POST /api/v1/egress/ca/rotate` | admin, person, browser | `admin_required` · `person_required` · `dashboard_required` · `400 invalid_ca` · `409 ca_unconstrained` · `409 rotating` · `409 ca_excludes` |
| internal: ALS receiver, `GET /egress/policy`, `POST /egress/dropped`, `GET /egress/route-value`, `GET /egress/leaf` | Envoy / egress-control only | refused from the app pool; unauthenticated |

**Audit rows:** `egress.set`, `egress.mode.set`, `egress.allow`/`egress.disallow`, `egress.request`
(outcome, actor `canyon`), `egress.request.dismiss`, `egress.route.set`/`.remove` (never a value;
from-secret's names the secret and the destination), `egress.networks.recreate`, `egress.ca.rotate`.
Every `403` refusal is one `denied` row: a level refusal's as every route's (contracts/routes.md, the
audit read), and a `person_required` or `dashboard_required` one.

## 10. Threat model

- **Stops:** data or a credential sent to a destination nobody allowed (layer 2); a credential
  leaving the app, which never holds it (layer 3); the door reaching the VPC, IMDS or Canyon itself;
  a client that ignores the proxy; DNS tunnelling on Docker; a forged caller (Docker: no raw sockets
  on a per-app network; Kubernetes layer 3: a pod-bound token); an app steering itself into
  interception; Canyon's own sidecars cut off by egress.
- **Does not stop:** exfiltration through an allowed destination that stores or echoes data, or
  echoes the injected header back; DNS tunnelling on Kubernetes until its PR; domain fronting through
  an allowed CDN on pass-through; an app reaching its front door's own endpoints (platform
  destinations); on Kubernetes under `CANYON_KUBE_SIGNIN_DIRECT`, any container of a floored pod reaching
  the listed addresses — or, inside the cluster, the provider's pods the operator's policy admits — on 443,
  unrecorded, and a removed or narrowed setting staying open to a running app until its next deploy (§2.2); a compromised Envoy pod, which holds cached values and leaves; tunnels open up to
  15 min past a narrowing; a misattributed record during IP reuse (layers 1–2, Kubernetes); the path and body of an
  absolute-form request crossing the pod network in clear to Envoy (the client's choice; the app holds
  no credential, so none is exposed — Envoy adds it and sends it only inside TLS).

## 11. Testing

Every new test red before green (rule 2).

- **Layer 1, unit:** `TestALSPathNeverStoredOrLogged` (a token planted in a query string is found
  nowhere), `TestAmbiguousOrTerminatingIPIsUnknown`, `TestFlushUpsertsOneRowPerDestination`,
  `TestCallBeforeMidnightFlushedAfterIsRecorded` (#2221), `TestNewDestinationPastCapCountsOverCap`,
  `TestContinuingGapExtendsItsRow`, `TestUnseenDestinationAgesOut`,
  `TestRunPinsTargetAddresses`, `TestNoProxyCoversEveryInNetworkURL`,
  `TestPatchRunCarriesTheRecordedEgressSpec`, `TestBootstrapHasNoTCPAdminAndNoFileLog`, CI
  `envoy --mode validate`. **Docker E2E** (`make demo-check`): raw dial to a public address fails;
  `/fetch` through Envoy yields a record with `run`; `nslookup example.com` fails;
  `CONNECT 169.254.169.254:80` and `CONNECT canyon-server:80` refused. **EKS test cluster:** f-cluster gains
  `/fetch?url=` using the language's own client (`/probe` stays a raw dial): `/fetch` → a row; raw
  `/probe` public → timeout; **I1 flips** (`$E2E_CANYON_URL:443` connected → timeout; CASES.md
  edit); choice 1 (alb) and choice 2 pods start with the floor on, choice 1 oidc on kind; every overlay's
  base holds the egress manifest. A test cluster holding other people's apps gets no fresh install, so
  `make e2e-cluster-egress` (CASES.md O1, #2101) proves the same with one app of its own: its logs
  say what its `fetch` and raw dial reached, and its records hold the row.
- **Layer 2:** canonicalisation table tests; `TestCheckRequestNeverLogged` (a token planted in a
  query string is in no log line of egress-control); `TestServesLastPolicyWhenServerDown`,
  `TestDeniesUnknownEnvEvenWhenStale`, `TestVersionEpochNeverGoesBackward`,
  `TestRunWaitsForEveryReplica`, `TestRefusesIPLiteralInObserve`,
  `TestKindChoice1OIDCPodStartsUnderEmptyAllowlist` (and `…ALB…`, `TestKindChoice2…`). **Bed,
  Playwright:** empty allowlist → `/fetch` `403` with `Canyon-Egress` → a pending request → a CLI
  `PUT` answers `dashboard_required` → Allow in the browser → `/fetch` `200` within 15 s → audit rows
  → `prod.egress-allowed` on blocks promotion naming the destination.
- **Layer 3:** e2e/echo, an h2 + gRPC echo reporting headers, ALPN, SNI and client-certificate
  fingerprint (#2146); the injected header arrives and the placeholder never; forced h2; grpc-go unary and
  server-streaming with metadata and trailers; a destination without a route shows the echo's own
  certificate fingerprint to the app and the app's own client certificate to the echo (never decrypted;
  `e2e/layer3_test.go`, the Docker stack; the test cluster's case is #2815); per-SDK fixtures — `@aws-sdk/client-s3`, the OpenAI
  SDK, axios 1.7 (absolute-form) and ≥ 1.19 (`CONNECT`), grpc-go `NewClient`, Spring WebClient, boto3 — on x86 Docker CI and the test cluster (kube CI is
  arm64 with kpack scaled to zero), each either passing or its limit written into §4.4.

## 12. Deliberately left out

Canyon's own proxy · ext_authz's HTTP flavour and non-Envoy forward-auth dialects · transparent
capture (needs `NET_ADMIN` in app pods) · internal destinations (private addresses; later, on their
own Envoy Deployment admitted only to those CIDRs) · OAuth grants (later, through the door) · ports
other than 80/443, non-HTTP, IPv6 destinations — a known gap: an app routed through Canyon reaches no
external database on another port · decrypting any destination without a route · a
per-app `off` · destinations declared in `canyon.toml` · RDS · TLS between the forwarder and Envoy
· E2, E3, E4 and Layer 4 until their triggers (§5) · hand-off egress · paths in records.

## 13. Implementation plan

PRs off `main`, ≤~750 added lines, contracts first and merged before any consumer (rule 5), one
compose service per PR, red before green. design/PERMISSIONS.md's *Outbound* change lands before
layer 2's server PR. Migrations are named by what they do, never by number.

**Layer 1 — See (~3,750, 9 PRs)**

| # | PR | ~Lines | Red first |
|---|---|---|---|
| 1 | contracts: `/run`/`/job` `egress` + `run`, Docker plan and labels, `POST /callers`, network recreation, records schema with `run` and flags, the ALS receiver | 400 | `validate.py` on a record carrying `path` |
| 2 | compose `egress` namespace holder + iptables rules + `canyon-egress-ctl` | 200 | `TestConnectIMDSRefused`, `TestConnectCanyonServerRefused` |
| 3 | Envoy bootstrap + compose Envoy service + CI validate | 350 | `TestBootstrapHasNoTCPAdminAndNoFileLog` |
| 4 | server: ALS receiver, schema drop, attribution via `/callers`, records (§2.4), caps | 550 | `TestALSPathNeverStoredOrLogged`, `TestAmbiguousOrTerminatingIPIsUnknown` |
| 5 | server: `via`, egress spec on the deploy row, patch and rollback carry it | 400 | `TestPatchRunCarriesTheRecordedEgressSpec` |
| 6 | Docker executor: `--internal`, `--ip-range`, slots, labels, variables, `NO_PROXY`, `/callers`, recreation | 400 | `TestRunPinsTargetAddresses`, `TestNoProxyCoversEveryInNetworkURL` |
| 7 | Kubernetes executor + `deploy/k8s/egress.yaml`: floor, Envoy's policy, proxy variables on three containers, `/callers` (#2097); the server's `via` and receiver on Kubernetes (#2286) | 550 | `TestKindAppReachesOnlyEnvoy`, choice 1 oidc/alb and choice 2 pods start |
| 8 | screens + CLI: Outbound card, Settings › Outbound, `status` row | 500 | `status` golden; card absent below contributor |
| 9 | E2E: f-cluster `/fetch`, I1, A5, I5, O1 | 400 | §11 layer-1 test cluster cases |

**PR 4 redone (#2222):** one row per destination (§2.4) — one PR, the contract commit first: the
migration, the receiver's upsert, cap, gap extension and age, attribution as a step (§2.3), the read.

**Layer 2 — Restrict (~4,400, 9 PRs; Kubernetes DNS ~350 later)**

| # | PR | ~Lines | Red first |
|---|---|---|---|
| 10 | contracts: modes, allowlist, requests, policy pull (epoch:seq), platform destinations, caps, refusal bodies | 450 | `validate.py` on an IP-literal entry |
| 11 | canonicalisation package | 300 | the table tests |
| 12 | server: policy and request routes, permissions, platform destinations from the front door | 700 | `TestAllowFromCLISessionIsDashboardRequired`, `TestReadOnlyLearnsNothing` |
| 13 | server: authenticated policy endpoint, epoch, caller refetch, replica tracking, convergence wait | 550 | `TestVersionEpochNeverGoesBackward`, `TestRunWaitsForEveryReplica` |
| 14 | egress-control: ext_authz, 30 s static stability, refetch, the rate limit's share over `known[]`, denial log (never a CheckRequest), the readiness probe; the receiver reads the check's metadata and files egress requests | 750 | `TestServesLastPolicyWhenServerDown`, `TestDeniesUnknownEnvEvenWhenStale`, `TestCheckRequestNeverLogged` |
| 15 | sidecar wiring: Envoy ext_authz on localhost, PDB, spread, the readiness exec probe, compose control service (#2119) | 400 | `TestControlDownAnswers503WithBody`, choice 1/2 under an empty allowlist |
| 16 | snapshot: `prod.egress-allowed` (seam 5 alone) | 120 | snapshot CI on a default other than `off` |
| 17 | receipt, screens, CLI: Allow/Dismiss, modes, `--format ai`, dismissed refusals on rollback | 700 | receipt names the destination; ai golden |
| 18 | E2E: the Playwright Allow flow | 450 | §11 layer-2 test cluster case |

**Layer 3 — Inject (~5,200, 10 PRs)**

**The spike — settled** (#2132; report in a comment on it; configs on the never-merged
branch `spike/egress-envoy-intercept`, `spike/envoy.yaml`, rerun by `spike/run.sh`). Stock
`envoyproxy/envoy:v1.39.3@sha256:dd85940439de19a0b6ae8419610363ea0ad351d9a994ea007161c206ec1e1865`
(BoringSSL), a Go ext_authz v3 + ALS stub, a Go echo upstream with its own CA; curl 8.22, axios
1.19.0 and 1.7.9 on node 22. Layer 3's interception works as designed, with the changes already
written into §2.1, §2.4, §4.2, §4.4 and §10.

1. **`cluster_header` routes a terminated `CONNECT` after ext_authz clears the route cache — yes.**
   Proved by :3128's route pair (`connect_matcher` + `x-canyon-intercept` present → `cluster_header`,
   else `dfp_raw`) under ext_authz `clear_route_cache: true`, `failure_mode_allow: false`: curl
   reached the upstream with the injected header; a remove-only OK also re-routed (the probe route
   `x-spike-route: stale` → `418`). The RBAC `DENY` on `x-canyon-intercept` answered a forged header
   `403` before any check; the same listener without it (:3129) let the header reach the ext_authz
   cluster and another env's intercept path. An intercept value naming no cluster is `503` (`NC`).
2. **`context_extensions` reach the inner check on an internal listener — yes.** Proved by
   `ix_claims_prod`'s `typed_per_filter_config` (`ExtAuthzPerRoute.check_settings.context_extensions`
   `{hop, app, env, destination}`), seen in the inner CheckRequest over HTTP/2 and HTTP/1.1; a
   mismatched `:authority` was `421`. The inner hop's source is `internal_client_address` — the app's
   address is gone (§2.4).
3. **Absolute-form `https://` passes — yes, and its path reaches ext_authz and ALS.** Proved by
   the `:scheme https` → `dfp_tls` route (`auto_sni`, `auto_san_validation`, `max_session_keys: 0`):
   axios 1.7.9 got a 200 with the header ext_authz #1 injected; `http://` to the inject destination
   was `403 inject_requires_tls`; a forged `Host` was dropped for the URL's authority. axios 1.19.0
   sends `CONNECT`. Every non-`CONNECT` ALS entry and every CheckRequest carried the path and query,
   which `http_grpc` cannot omit (§2.4).
4. **`internal_upstream` metadata passthrough replaces the per-env listeners — yes, but not the
   per-env clusters; filter state replaces both.** Proved by `ixs_shared`: `passthrough_metadata`
   from host metadata gave one listener but still one cluster per env (`ixs_claims_prod`,
   `ixs_claims_preview` — static metadata, a CDS push per env); `set_filter_state` with
   `shared_with_upstream: ONCE` and `header_mutation` on the one cluster `ixfs` carried
   `claims/prod` to the inner check over the app's forged `x-canyon-caller`, and the inner ALS entry
   logged `canyon.caller`; a hop that skipped ext_authz #1 carried `-` in its access-log entry (to the check its
   header is empty, #2140's review). That is §4.2's design.

Also seen: a pass-through destination's certificate reached the client untouched (same fingerprint
as the upstream's own), and only inject destinations were re-signed; `CONNECT … HTTP/1.0` is `426`.

| # | PR | ~Lines | Red first |
|---|---|---|---|
| 19 | contracts: value routes, `/run` `egress.ca`, trust variables, intercept names, route-value fetch | 450 | `validate.py` on a route read carrying a value |
| 20 | server: sealed tables, root, name-constrained intermediate, leaves, BYO sub-CA, routes, from-secret | 750 | `TestIntermediateConstrainedToInjectDestinations`, `TestNoRouteAnswersAValue` |
| 21 | egress-control: xDS (LDS/SDS) from the policy — no CDS, no per-env listeners or clusters | 500 | `TestTwoReplicasServeIdenticalSnapshots` |
| 22 | egress-control: inner check, caller from `x-canyon-caller` (empty refused), overwrite, `421`, `inject_requires_tls`, value cache, the absolute-form inject branch | 650 | `TestMisdirectedAuthorityDenied`, `TestInjectRequiresTLS`, `TestInnerHopWithoutCallerRefused` |
| 23 | Envoy layer 3: RBAC before ext_authz, `set_filter_state`, the one internal cluster, `header_mutation`, upstream TLS | 250 | `TestForgedInterceptHeaderRejected`, `TestRemoveOnlyResponseReroutes` |
| 24 | Kubernetes: per-env ServiceAccount, projected token, forwarder (web and job) | 550 | `TestKindCallWithoutTokenRefused` |
| 25 | executors: CA mount, subject-hash names, trust variables, `JDK_JAVA_OPTIONS` append | 350 | `TestJavaLoaderReadsTheHashName` |
| 26 | per-SDK fixtures | 700 | each fixture's call through an inject route |
| 27 | screens: routes, keys, BYO | 550 | the route form never echoes a value |
| 28 | E2E: injection, h2, gRPC, never-decrypted fingerprint | 450 | `TestPassThroughDestinationIsNeverDecrypted` |

**Later:** Kubernetes DNS closure (~350) · E2 (~400) · the Istio kind spike, then E3 (~700) · E4
(~300) · Layer 4 (~1,200). About 13,350 lines through layer 3.

## 14. The owner's decisions

**Settled:** Envoy data plane, Canyon control plane; egress-control as Envoy's sidecar; static
stability; ALS-only layer 1; layer 1 keeps one row per destination, never a time series (2026-10-13,
#2222); no X.509-SVIDs from Canyon; value routes first; E2/E3/E4/Layer 4 later.

1. **Token verification — decided** 2026-11-05 (the owner): offline against the cluster's
   service-account key set (no per-call API dependency, no `system:auth-delegator`; a deleted pod's
   token lives ≤10 min), not TokenReview.
2. **Who widens — settled** 2026-10-11 (the owner): contributors, dashboard only (as approving
   a production approval); agents never (design/PERMISSIONS.md, *Outbound*).
3. **Caps — decided** 2026-11-05 (the owner): 500 destinations per app env (rows, aged out after 30
   days unseen, decided 2026-10-14), 30 new names per minute, 50 pending requests, each of them
   configurable. These numbers are the defaults; an admin sets each in Settings › Outbound, `destinations`
   50 to 5,000, `new_names` 1 to 600 and `pending` 1 to 500 (ranges decided 2026-11-09, #2691).
4. **Tunnel lifetime — decided** 2026-11-05 (the owner): `max_stream_duration` stays 15 minutes, how
   long a narrowing takes to reach an open WebSocket or gRPC stream, as the allowlist is checked only
   when a connection opens.
5. **Istio attribution's test (#2222):** the issue asks for an Istio-shaped entry attributed by
   workload. The step needs the per-env ServiceAccounts (layer 3) and a mesh listener (the Istio
   spike), so built now it is code only a test calls. **Decided** 2026-10-14:
   #2222 makes attribution the step, by address alone; by workload, with its red test, lands
   with E3 (#2231).
6. **`run`:** the records keep none (§2.4), so nothing in layer 1 reads `/run`'s `run` or
   `canyon.gate_run`. **Decided** 2026-10-14: kept (seam 4, built) until layer 2; dropped then, in a
   seam PR, if nothing reads them.

## 15. Edits other docs need (listed, not made)

- **design/JOBS.md §6** — the exact changes are in this revision's report; in short: the
  forwarder-only sidecar and the per-env ServiceAccount arrive with EGRESS **layer 3**, not layer 1;
  layers 1–2 attribute job pods by pod IP via `POST /callers` and Docker job runs by the `.14`/`.15`
  slot and `canyon.run` label, with no egress state to write when jobs are armed; `/job` gains
  `egress {proxy, no_proxy[], ca?}`.
- **design/PERMISSIONS.md** — made, first, in its own PR (#2082): *Outbound* (§3.5).
- **design/PATCHING.md** — the run-config fingerprint's egress part is the deploy row's recorded
  spec (§2.5). **design/OPERATIONS.md** — made with #2125: rollback lists refusals of dismissed destinations.
- **design/ARCHITECTURE.md** — the floor (*On Kubernetes*, the Docker paragraph); a pointer here.
- **design/HAND-OFF.md** — egress is `egress_external` under `run_by: org`.
- **contracts/** — §9's seams. **e2e/cluster/CASES.md** — I1 flips; the egress cases.
- **design/EXPERIENCE.md D5** — §8.4 once accepted · **design/specs/cli.md** §6.1 — `outbound` ·
  **the shipped skill** — the HTTPS_PROXY sentence · **deploy/k8s/README.md** — the egress manifest,
  the state certificate install step, the cluster's service-account key set.
