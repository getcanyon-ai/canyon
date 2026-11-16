---
title: One k3s host, step by step
description: A worked example of the Kubernetes install on one Linux host running k3s, with the registry, both Postgres servers and the gateway in the cluster.
---

| | |
|---|---|
| **Time** | One to two hours, run as root on a fresh host |
| **You need** | A Linux host with a public IPv4 address, 2 vCPU and 4 GB at least, and about 60 GB of disk · a domain with DNS you control · an OIDC client at your identity provider · `crane`, `openssl` and `curl`. Details: [What you need](#what-you-need) |
| **You get** | Canyon at `https://<domain>`, signing people in through your identity provider, and apps at `https://<app>.preview.<domain>` and `https://<app>.prod.<domain>`, with the registry, both Postgres servers and the gateway in the cluster |
| **Backups** | [On one k3s host](/self-host/backup-and-upgrade/#on-one-k3s-host): both Postgres servers, the key and the code, through `kubectl` |
| **Not covered** | A second server: while this one is down, so are Canyon and every app ([Caveats](#caveats)) |

A worked example of [Install on Kubernetes](/self-host/kubernetes/)'s install on one Linux host running k3s. It covers
everything that guide says *you bring*: a registry, both Postgres servers and a gateway with TLS,
all in the cluster. It is written for Ubuntu 24.04 with k3s v1.36, Calico v3.32.2 and kpack v0.18.0.
Read *Caveats* before exposing the host. The front door, its two ways of signing people in and the
address form are defined in [How people reach apps](/self-host/front-door/).

```mermaid title="One k3s host: Canyon and everything the install guide says you bring, on one Linux server"
flowchart TD
  people["People and coding agents<br>your domain and its app wildcards<br>point at the host"]
  idp["Your identity provider<br>an OIDC client"]
  subgraph host["One Linux host: k3s with Calico"]
    caddy["Caddy, the gateway<br>ports 80 and 443"]
    canyon["Canyon<br>its four namespaces"]:::canyon
    kpack["kpack"]
    reg["Registry<br>canyon-infra"]
    pg[("Canyon's Postgres, 5432<br>canyon-infra")]
    apg[("The apps' Postgres, 5433<br>canyon-infra")]
  end
  people -->|"https"| caddy
  caddy --> canyon
  canyon -->|"sign-in"| idp
  canyon --> pg
  canyon --> apg
  canyon --> kpack
  kpack --> reg
```

## What you need

| | |
|---|---|
| A Linux host with root, a public IPv4 address, and ports 80 and 443 reachable | k3s's ServiceLB publishes the gateway on the node's own 80/443. The install below disables k3s's Traefik, because the gateway is Caddy. The pod network is Calico's default, `192.168.0.0/16`, so the host must have no address in it. |
| 2 vCPU and 4 GB RAM at least (4 vCPU and 8 GB recommended), and about 60 GB of disk | Idle, the stack uses about 1.7 GB. Each app build runs a kpack pod, which is CPU- and memory-hungry. The volumes below claim 10 Gi (server) + 20 Gi (registry) + 10 Gi + 10 Gi (Postgres) + 1 Gi (gateway), plus 5 Gi per scanner database. Images and the build cache come on top. |
| [crane](https://github.com/google/go-containerregistry/tree/main/cmd/crane), openssl, curl | crane copies Canyon's five images and its two scanner images from the release into the cluster's registry. k3s bundles `kubectl`, with kustomize built into `kubectl apply -k`. |
| A domain with DNS you control | An A record for `<domain>`, plus A records `*.preview.<domain>` and `*.prod.<domain>` (or one `*.<domain>`), all pointing at the host. Caddy issues each certificate over HTTP-01, so DNS must resolve before the first request. |
| An OIDC client | Redirect URI `https://<domain>/auth/callback`. The issuer, client ID and client secret (if any) go into the server's Secret (step 5). |

Run every step as root (`sudo -i`): the kubeconfig k3s writes is readable by root alone. Set these
once, in the shell you run every step from. `RELEASE` is the Canyon release you install, one of
[Canyon's releases](https://github.com/getcanyon-ai/canyon-releases/releases): its
`canyon-deploy.tar.gz` holds the install base, `deploy/k8s`, and its `images.txt` names its images.

```sh
export DOMAIN=canyon.example.com      # Canyon's host; apps live at <app>.preview.$DOMAIN and <app>.prod.$DOMAIN
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
export RELEASE=v0.2.2                 # the release's tag
export WORK=$(mktemp -d)              # $WORK holds the release, your overlay and the manifests below; keep it
export CANYON=$WORK/canyon            # the release's canyon-deploy.tar.gz, unpacked
curl -fsSL https://github.com/getcanyon-ai/canyon-releases/releases/download/$RELEASE/canyon-deploy.tar.gz | tar -xz -C $WORK
curl -fsSL -o $WORK/images.txt https://github.com/getcanyon-ai/canyon-releases/releases/download/$RELEASE/images.txt
```

## 1. k3s

k3s runs with Calico in place of its own network plugin and policy controller. Calico applies each
pod's NetworkPolicy to all of its traffic, including traffic to the node itself, which k3s's
controller leaves open. CoreDNS carries `k8s-app: kube-dns`, and local-path is the default
StorageClass. Traefik is disabled, and containerd is told to pull from the in-cluster registry over
plain http. The registry's Service gets the fixed ClusterIP `10.43.254.10`, inside k3s's default
Service range `10.43.0.0/16`. Calico's manifest is checked against its digest, the one CI installs.

Pods resolve names through CoreDNS, which k3s points at the host's resolver. k3s skips a loopback
resolver, such as systemd-resolved's `127.0.0.53`, by itself. It doesn't skip one that only the host
can reach, such as Tailscale MagicDNS's `100.100.100.100`, and from a pod every lookup then times
out: Caddy gets no certificate and kpack pulls nothing. If the host's `/etc/resolv.conf` names such
a resolver, give k3s public ones (or your provider's) first. The step below picks the file up:

```sh
mkdir -p /etc/rancher/k3s && printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > /etc/rancher/k3s/resolv.conf
```

```sh
mkdir -p /etc/rancher/k3s
printf 'disable:\n  - traefik\nflannel-backend: none\ndisable-network-policy: true\ncluster-cidr: 192.168.0.0/16\nwrite-kubeconfig-mode: "0600"\n' > /etc/rancher/k3s/config.yaml
[ -f /etc/rancher/k3s/resolv.conf ] && echo 'resolv-conf: /etc/rancher/k3s/resolv.conf' >> /etc/rancher/k3s/config.yaml
printf 'mirrors:\n  "10.43.254.10:5000":\n    endpoint:\n      - "http://10.43.254.10:5000"\n' > /etc/rancher/k3s/registries.yaml
curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=stable sh -
curl -sSfL -o $WORK/calico.yaml https://raw.githubusercontent.com/projectcalico/calico/v3.32.2/manifests/calico.yaml
echo "a8c828a06a87c629a282ebbc424895b77f3a030251993e41ea400a743675bb02  $WORK/calico.yaml" | sha256sum -c -
kubectl apply -f $WORK/calico.yaml >/dev/null
kubectl -n kube-system rollout status ds/calico-node --timeout=5m
kubectl wait --for=condition=Ready node --all --timeout=5m
kubectl version | tail -1   # Server Version must be v1.29 or newer
kubectl run dnscheck --rm -i --restart=Never --image=docker.io/library/busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e -- nslookup ghcr.io   # an address, not "timed out"
```

Start from a host without k3s: an existing k3s keeps the network plugin it was installed with.
If anything else on the host already listens on 80 or 443, stop it first. Otherwise ServiceLB can't
publish the gateway (step 7).

### Close the host's ports

k3s leaves its API on 6443, the kubelet on 10250, and a NodePort (30000–32767) for every
LoadBalancer Service open to the internet, over IPv4 and IPv6. A NodePort reaches the gateway around
any filter on 80/443. Canyon's policies can't close these (*Caveats*), so this script does, and a
systemd unit re-applies it at every boot, before k3s starts:

- **Outside traffic** to them is dropped in the raw table, before kube-proxy's DNAT, by `iptables`
  and `ip6tables` alike.
- **Port 443** is public while `ALLOW_443` is empty. List IPv4 addresses, space-separated, to admit
  only those, and include the host's own public address: the front-door check (step 9) dials the
  app addresses from inside the cluster, and on a cloud host that traffic comes back in from it.
  The same goes for a cloud firewall in front of the host. A home connection's IP changes, and when
  it does you're locked out until you update the list.
- **Port 80** stays public for Caddy's HTTP-01 challenges.
- **`kubectl`** keeps working on the node itself. From elsewhere, go over SSH or a VPN, because 6443
  is closed on the public interfaces.

```sh
PUBLIC_IFACES=$(ip -o -4 route show default | awk '{print $5}' | sort -u | tr '\n' ' ')
echo "public interfaces: $PUBLIC_IFACES"   # add any other internet-facing interface (e.g. a private VPC one) to the script
cat > /usr/local/sbin/canyon-k3s-firewall.sh <<EOF
#!/bin/sh
set -e
PUBLIC_IFACES="$PUBLIC_IFACES"
ALLOW_443=""      # empty: 443 is public; else only these space-separated IPv4 addresses

for ipt in iptables ip6tables; do
  \$ipt -t raw -N CANYON-K3S-IN 2>/dev/null || \$ipt -t raw -F CANYON-K3S-IN
  \$ipt -t raw -A CANYON-K3S-IN -p tcp -m multiport --dports 6443,10250,30000:32767 -j DROP
  \$ipt -t raw -A CANYON-K3S-IN -p udp --dport 30000:32767 -j DROP
  for ifc in \$PUBLIC_IFACES; do
    \$ipt -t raw -C PREROUTING -i \$ifc -j CANYON-K3S-IN 2>/dev/null || \$ipt -t raw -I PREROUTING -i \$ifc -j CANYON-K3S-IN
  done
done
if [ -n "\$ALLOW_443" ]; then
  for ip in \$ALLOW_443; do iptables -t raw -A CANYON-K3S-IN -p tcp --dport 443 -s "\$ip" -j RETURN; done
  iptables -t raw -A CANYON-K3S-IN -p tcp --dport 443 -j DROP
fi
EOF
chmod 755 /usr/local/sbin/canyon-k3s-firewall.sh
cat > /etc/systemd/system/canyon-k3s-firewall.service <<'EOF'
[Unit]
Description=Canyon on k3s: keep the k3s API, kubelet and NodePorts private
Before=k3s.service
After=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/canyon-k3s-firewall.sh
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now canyon-k3s-firewall
iptables -t raw -S CANYON-K3S-IN && ip6tables -t raw -S CANYON-K3S-IN
```

To change the 443 allowlist later, edit `ALLOW_443` in the script and run
`systemctl restart canyon-k3s-firewall`. To find the IP a browser comes from, open
`https://ifconfig.me` in it.

## 2. kpack

This is the install guide's step 1, with the release checked against its digest:

```sh
curl -sSfL -o $WORK/kpack.yaml https://github.com/buildpacks-community/kpack/releases/download/v0.18.0/release-0.18.0.yaml
echo "cde8b7df8d31d6a5758ec4880eec45009f17811baf3df5a29b76a144fe200e69  $WORK/kpack.yaml" | sha256sum -c -
kubectl apply -f $WORK/kpack.yaml >/dev/null 2>&1 || true   # its own objects need its CRDs and webhook first
kubectl wait --for=condition=Established crd --all --timeout=2m
kubectl -n kpack rollout status deploy/kpack-webhook --timeout=5m
kubectl apply -f $WORK/kpack.yaml
```

## 3. Registry and Postgres

The install guide's steps 2–5 bring these; here they're pods in `canyon-infra`:

- **Registry:** `registry:2`, anonymous, with deletes on because the executor deletes an app's
  images. It sits at `10.43.254.10:5000`, an RFC 1918 address, which the executor and kpack speak
  plain http to.
- **Canyon's Postgres:** on 5432.
- **The apps' Postgres:** on 5433 (the install guide's step 4), with the admin login `canyon_admin`.

The servers run as pods, not host-networked, so NetworkPolicy covers them. The registry's policy
admits the executor, kpack's controller (it pushes the builder) and the build pods, and nothing else
in the cluster. The node itself pulls from it without a rule: Calico doesn't apply a pod's ingress
policy to its own node's traffic. Each database's password is a Secret made before the database
starts, in a block of its own.

```sh title="Make canyon-infra"
kubectl create namespace canyon-infra
# Pod Security restricted, as the chart's canyon-infra: the registry and Postgres run as no root
kubectl label namespace canyon-infra pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/warn=restricted pod-security.kubernetes.io/audit=restricted
for name in canyon-postgres app-postgres; do
  kubectl -n canyon-infra create secret generic $name --from-literal=password=$(openssl rand -hex 24)
done
```

Then the registry and both databases, each as no root under Pod Security `restricted`. This block is also the
one to apply again when Canyon's upgrade notes say so: it creates nothing that exists already and changes no
password.

```sh title="Run the registry and both databases"
cat > $WORK/infra.yaml <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: registry, namespace: canyon-infra}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 20Gi}}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: registry, namespace: canyon-infra}
spec:
  replicas: 1
  strategy: {type: Recreate}
  selector: {matchLabels: {app: registry}}
  template:
    metadata: {labels: {app: registry}}
    spec:
      securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, fsGroup: 65532, fsGroupChangePolicy: OnRootMismatch, seccompProfile: {type: RuntimeDefault}}
      initContainers: # starts the registry only once its user can write its volume, else says what to run
        - name: volume-owned
          image: docker.io/library/registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373
          command: [sh, -c, "d=/var/lib/registry; u=$(id -u); g=$(id -g); bad=$( { find \"$d\" -mindepth 1 -type d ! -user \"$u\" ! \\( -group \"$g\" -perm -070 \\); find \"$d\" -mindepth 1 ! -type d ! -user \"$u\" ! \\( -group \"$g\" -perm -060 \\); } | head -n 3 | tr '\\n' ' '); if [ -n \"$bad\" ] || ! touch \"$d/.canyon-writable\" || ! rm \"$d/.canyon-writable\"; then echo \"canyon: user $u can't write $d ($(echo ${bad:-$d})). Hand it to user $u as docs/self-host/backup-and-upgrade.md says (The upgrade that runs the chart's bundled parts as no root), then delete this pod.\" >&2; exit 1; fi"]
          securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
          volumeMounts: [{name: data, mountPath: /var/lib/registry}]
      containers:
        - name: registry
          image: docker.io/library/registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373
          env: [{name: REGISTRY_STORAGE_DELETE_ENABLED, value: "true"}]
          volumeMounts: [{name: data, mountPath: /var/lib/registry}]
          readinessProbe: {httpGet: {path: /v2/, port: 5000}}
          securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
      volumes: [{name: data, persistentVolumeClaim: {claimName: registry}}]
---
apiVersion: v1
kind: Service
metadata: {name: registry, namespace: canyon-infra}
spec:
  clusterIP: 10.43.254.10
  selector: {app: registry}
  ports: [{port: 5000, targetPort: 5000}]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: registry, namespace: canyon-infra}
spec:
  podSelector: {matchLabels: {app: registry}}
  policyTypes: [Ingress]
  ingress:
    - from:
        - {namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: canyon}}, podSelector: {matchLabels: {app.kubernetes.io/name: canyon-executor}}}
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: kpack}}
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: canyon-builds}}
      ports: [{protocol: TCP, port: 5000}]
EOF
for db in canyon-postgres:5432:canyon:canyon app-postgres:5433:canyon_admin:postgres; do
  IFS=: read -r name port user database <<<"$db"
  cat >> $WORK/infra.yaml <<EOF
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $name, namespace: canyon-infra}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 10Gi}}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: $name, namespace: canyon-infra}
spec:
  replicas: 1
  strategy: {type: Recreate}
  selector: {matchLabels: {app: $name}}
  template:
    metadata: {labels: {app: $name}}
    spec:
      securityContext: {runAsNonRoot: true, runAsUser: 70, runAsGroup: 70, fsGroup: 70, fsGroupChangePolicy: OnRootMismatch, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: postgres
          image: docker.io/library/postgres:16-alpine@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea
          args: ["-c", "port=$port"]
          env:
            - {name: POSTGRES_USER, value: $user}
            - {name: POSTGRES_DB, value: $database}
            - {name: POSTGRES_PASSWORD, valueFrom: {secretKeyRef: {name: $name, key: password}}}
            - {name: PGDATA, value: /var/lib/postgresql/data/pgdata}
          volumeMounts: [{name: data, mountPath: /var/lib/postgresql/data}]
          readinessProbe: {exec: {command: [pg_isready, -p, "$port", -U, $user, -d, $database]}, periodSeconds: 5}
          securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
      volumes: [{name: data, persistentVolumeClaim: {claimName: $name}}]
---
apiVersion: v1
kind: Service
metadata: {name: $name, namespace: canyon-infra}
spec:
  selector: {app: $name}
  ports: [{port: $port, targetPort: $port}]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: $name, namespace: canyon-infra}
spec:
  podSelector: {matchLabels: {app: $name}}
  policyTypes: [Ingress]
  ingress: [{from: [{ipBlock: {cidr: 192.168.0.0/16}}], ports: [{protocol: TCP, port: $port}]}]
EOF
done
kubectl apply -f $WORK/infra.yaml
for d in registry canyon-postgres app-postgres; do kubectl -n canyon-infra rollout status deploy/$d --timeout=5m; done
```

Each database's policy admits the pod network (`192.168.0.0/16`) on its port and nothing else.
What keeps apps off Canyon's Postgres is their own policy, which the executor writes and which opens
only 5433 inside the cluster.

## 4. Canyon's images

These are Canyon's five images the install guide names, plus its Opengrep and ESLint scanner images,
copied from the release's `images.txt` into the registry through a port-forward. crane copies each
image whole, for both architectures, so its digest stays the one `images.txt` names, and it speaks
plain http to `localhost` without extra configuration. Every image is named by digest, as the base
requires. Each digest is read back from the registry, not from `images.txt`, so an image the
registry doesn't hold shows up as a gap. The loop stops at the first image that fails.

```sh
kubectl -n canyon-infra port-forward svc/registry 5001:5000 >/dev/null & PF=$!; sleep 3
(set -e
for name in canyon-server canyon-executor-kube canyon-identity canyon-run canyon-build canyon-opengrep canyon-eslint; do
  crane copy "$(grep "/$name@" $WORK/images.txt)" localhost:5001/$name:$RELEASE
done)
digest() { crane digest localhost:5001/$1:$RELEASE; }
export SERVER_DIGEST=$(digest canyon-server) EXECUTOR_DIGEST=$(digest canyon-executor-kube) IDENTITY_DIGEST=$(digest canyon-identity) RUN_DIGEST=$(digest canyon-run) BUILD_DIGEST=$(digest canyon-build)
export OPENGREP_DIGEST=$(digest canyon-opengrep) ESLINT_DIGEST=$(digest canyon-eslint)
kill $PF
echo $SERVER_DIGEST $EXECUTOR_DIGEST $IDENTITY_DIGEST $RUN_DIGEST $BUILD_DIGEST $OPENGREP_DIGEST $ESLINT_DIGEST   # seven sha256:…; fewer: copy the missing image again
```

The cluster pulls these as `10.43.254.10:5000/<name>@<digest>`: the same registry and the same
digests.

## 5. Secrets

These are the install guide's step 3's `canyon-registry` and the Secret `canyon-server`. The registry takes no
login, but kpack needs a docker-config Secret to push with, so any user and password will do. The
server's Secret holds the database URLs and your OIDC client. Add `CANYON_OIDC_CLIENT_SECRET` if
your client has one. Leave `CANYON_OIDC_ALLOWED_DOMAINS` and `CANYON_OIDC_ALLOWED_EMAILS` unset: the
first sign-in asks who may sign in. Set in the Secret instead, a list is fixed and the setup screen shows it read-only. With both set, Canyon refuses to start unless every `CANYON_ADMINS` address is on one of them; otherwise setup can't finish until at least one admin is on either list.

`CANYON_TRUSTED_PROXIES` is the one address Canyon believes about who is asking: Caddy's, the
gateway step 7 runs as a pod. A pod's address changes when it restarts, and the install guide
forbids listing a pod range there, so step 7 pins Caddy's address with Calico, which step 1
installs, and this Secret trusts that address alone. `CADDY_IP` is any address in the pod network,
`192.168.0.0/16`, that no other pod has. Canyon then sees each person's own address: each sign-in
has a rate limit of its own, the device page's "Requested from" shows the browser's address, and
only Caddy may ask Canyon which names get a certificate (step 7).

```sh title="Create the Secrets"
CADDY_IP=192.168.255.200   # Caddy's pod address, pinned in step 7
kubectl apply -f $CANYON/deploy/k8s/namespaces.yaml
kubectl -n canyon-builds create secret docker-registry canyon-registry \
  --docker-server=10.43.254.10:5000 --docker-username=canyon --docker-password=anonymous
pw() { kubectl -n canyon-infra get secret $1 -o jsonpath='{.data.password}' | base64 -d; }
kubectl -n canyon create secret generic canyon-server \
  --from-literal=CANYON_DATABASE_URL="postgres://canyon:$(pw canyon-postgres)@canyon-postgres.canyon-infra.svc.cluster.local:5432/canyon?sslmode=disable" \
  --from-literal=CANYON_APP_DATABASE_URL="postgres://canyon_admin:$(pw app-postgres)@app-postgres.canyon-infra.svc.cluster.local:5433/postgres?sslmode=disable" \
  --from-literal=CANYON_PUBLIC_URL="https://$DOMAIN" \
  --from-literal=CANYON_TRUSTED_PROXIES="$CADDY_IP/32" \
  --from-literal=CANYON_OIDC_ISSUER="https://<your issuer>" \
  --from-literal=CANYON_OIDC_CLIENT_ID="<your client id>" \
  --from-literal=CANYON_ADMINS="<you@example.com>"
kubectl -n canyon create secret generic canyon-egress --from-literal=token="$(openssl rand -hex 32)"
```

`canyon-egress` holds the token Canyon's egress gateway sends the server's receiver. The server and the
gateway both read it, and neither starts without it.

The server creates its schema and runs its migrations when it boots. A Kubernetes install starts
with an empty fleet: nothing is seeded. Without a model, the AI review shows as skipped. To enable it,
add `ANTHROPIC_API_KEY` (or `CANYON_MODEL_URL`, see [The AI model](/org/set-up/#the-ai-model)) to this Secret.

## 6. The overlay

This is the install guide's *Then write your overlay*, with this cluster's values:

- **Images:** by digest.
- **The executor's environment.**
- **The builder's tag.**
- **The database CIDRs:** set to the pod network. Policies match after kube-proxy has rewritten a
  Service address to a pod's, so a CIDR has to name the pods, never the Service. The whole pod
  network is acceptable only on a dedicated host: any pod listening on 5433 is then reachable from
  every app.
- **Two egress rules the base can't know:** the executor to the API server, which k3s serves on
  the node at 6443 (the install guide's step 3), and the executor and kpack's build pods to the registry. The
  base lets build pods reach public addresses only.

```sh
API_IP=$(kubectl get endpointslice -n default -l kubernetes.io/service-name=kubernetes -o jsonpath='{.items[0].endpoints[0].addresses[0]}')
mkdir -p $WORK/overlay && cat > $WORK/overlay/k3s-policy.yaml <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: canyon-executor-k3s, namespace: canyon}
spec:
  podSelector: {matchLabels: {app.kubernetes.io/name: canyon-executor}}
  policyTypes: [Egress]
  egress:
    - to: [{ipBlock: {cidr: $API_IP/32}}]
      ports: [{protocol: TCP, port: 6443}]
    - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: canyon-infra}}, podSelector: {matchLabels: {app: registry}}}]
      ports: [{protocol: TCP, port: 5000}]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: canyon-builds-k3s, namespace: canyon-builds}
spec:
  podSelector: {}
  policyTypes: [Egress]
  egress:
    - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: canyon-infra}}, podSelector: {matchLabels: {app: registry}}}]
      ports: [{protocol: TCP, port: 5000}]
EOF
cat > $WORK/overlay/kustomization.yaml <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - $(realpath --relative-to=$WORK/overlay $CANYON/deploy/k8s)
  - k3s-policy.yaml
images:
  - {name: canyon-server, newName: 10.43.254.10:5000/canyon-server, digest: "$SERVER_DIGEST"}
  - {name: canyon-executor-kube, newName: 10.43.254.10:5000/canyon-executor-kube, digest: "$EXECUTOR_DIGEST"}
patches:
  - target: {kind: Deployment, name: canyon-executor}
    patch: |-
      [{"op": "replace", "path": "/spec/template/spec/containers/0/env", "value": [
        {"name": "CANYON_KUBE_REGISTRY", "value": "10.43.254.10:5000"},
        {"name": "CANYON_IMAGE_IDENTITY", "value": "10.43.254.10:5000/canyon-identity@$IDENTITY_DIGEST"},
        {"name": "CANYON_IMAGE_OPENGREP", "value": "10.43.254.10:5000/canyon-opengrep@$OPENGREP_DIGEST"},
        {"name": "CANYON_IMAGE_ESLINT", "value": "10.43.254.10:5000/canyon-eslint@$ESLINT_DIGEST"},
        {"name": "CANYON_KUBE_DB_CIDRS", "value": "192.168.0.0/16"}]}]
  - target: {kind: Deployment, name: canyon-server} # a merge by name: the sidecar's image, the executor's value
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata: {name: canyon-server, namespace: canyon}
      spec:
        template:
          spec:
            containers:
              - name: server
                env:
                  - {name: CANYON_IMAGE_IDENTITY, value: "10.43.254.10:5000/canyon-identity@$IDENTITY_DIGEST"}
  - target: {kind: ClusterStack, name: canyon} # every app builds on canyon-build and runs on canyon-run
    patch: '[{"op": "replace", "path": "/spec/runImage/image", "value": "10.43.254.10:5000/canyon-run@$RUN_DIGEST"}, {"op": "replace", "path": "/spec/buildImage/image", "value": "10.43.254.10:5000/canyon-build@$BUILD_DIGEST"}]'
  - target: {kind: ClusterBuilder, name: canyon}
    patch: '[{"op": "replace", "path": "/spec/tag", "value": "10.43.254.10:5000/canyon-builder"}]'
  - target: {kind: NetworkPolicy, name: canyon-server}
    patch: |-
      [{"op": "replace", "path": "/spec/egress/3/to/0/ipBlock/cidr", "value": "192.168.0.0/16"},
       {"op": "replace", "path": "/spec/egress/4/to/0/ipBlock/cidr", "value": "192.168.0.0/16"},
       {"op": "replace", "path": "/spec/egress/5/to/0/ipBlock/cidr", "value": "$API_IP/32"}]
EOF
kubectl apply -k $WORK/overlay
for d in canyon-executor canyon-server canyon-router; do kubectl -n canyon rollout status deploy/$d --timeout=10m; done
# The certificate the server minted for the policy Canyon's egress check pulls, published for it to pin.
cert=$(kubectl -n canyon exec deploy/canyon-server -- /canyon-server egress-policy-cert) &&
  kubectl -n canyon create configmap canyon-egress-policy-cert --from-literal=tls.crt="$cert" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n canyon rollout status deploy/canyon-egress --timeout=10m # its pods start once the certificate is published
```

`egress[5]` is the API server at `$API_IP`, from the executor's policy above: the server reads your cluster's
service-account key set there, to check the egress gateway's token. Calico, which enforces NetworkPolicy here
(step 1 turns off k3s's own), matches after kube-proxy has rewritten the `kubernetes` Service's cluster IP to
the endpoint, so the endpoint alone suffices here, without the cluster IP the
[install guide](/self-host/kubernetes/#install)'s overlay adds.

## 7. The gateway

The gateway is Caddy on the node's 80 and 443. Canyon's host goes to `canyon-server:7777` and the
app wildcards to `canyon-router:80`, as the install guide's *Next, point your gateway at Canyon* says.

A wildcard certificate would need your DNS provider's API. Instead, Caddy issues one certificate
per app host on its first visit, over HTTP-01. Before each, it asks Canyon, whose answer is yes
only for Canyon's own host, the address of an app that exists, and the two `canyon-probe` addresses
the front-door check dials. So nobody can make Caddy request certificates for made-up app names and
spend Let's Encrypt's weekly limit, which is per registered domain: `example.com` as a whole,
shared with everything else your company runs under it. Canyon answers that question for Caddy
alone, at the address step 5 trusts: the `cni.projectcalico.org/ipAddrs` annotation below pins
Caddy's pod to `$CADDY_IP`, and the `IPReservation` keeps Calico from handing that address to any
other pod (the annotation still takes it). Every certificate is public: Certificate Transparency logs list each
app's name the first time it is opened. So give Canyon a domain used for nothing else, or use one
wildcard certificate per environment from your DNS provider's API (DNS-01) instead.

```sh title="Run the gateway"
cat > $WORK/gateway.yaml <<EOF
apiVersion: crd.projectcalico.org/v1
kind: IPReservation
metadata: {name: canyon-gateway-caddy}
spec: {reservedCIDRs: ["$CADDY_IP/32"]}
---
apiVersion: v1
kind: Namespace
metadata:
  name: canyon-gateway
  labels: {pod-security.kubernetes.io/enforce: restricted, pod-security.kubernetes.io/warn: restricted, pod-security.kubernetes.io/audit: restricted}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: caddy, namespace: canyon-gateway}
data:
  Caddyfile: |
    {
    	on_demand_tls {
    		ask http://canyon-server.canyon.svc.cluster.local:7777/tls/ask
    	}
    }
    $DOMAIN {
    	reverse_proxy canyon-server.canyon.svc.cluster.local:7777
    }
    *.preview.$DOMAIN, *.prod.$DOMAIN {
    	tls {
    		on_demand
    	}
    	reverse_proxy canyon-router.canyon.svc.cluster.local:80
    }
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: caddy-data, namespace: canyon-gateway}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 1Gi}}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: caddy, namespace: canyon-gateway}
spec:
  replicas: 1
  strategy: {type: Recreate}
  selector: {matchLabels: {app: caddy}}
  template:
    metadata:
      labels: {app: caddy}
      annotations: {cni.projectcalico.org/ipAddrs: '["$CADDY_IP"]'}
EOF
# the rest of Caddy's Deployment as written, its start check's $ signs its own, not the shell's
cat >> $WORK/gateway.yaml <<'EOF'
    spec:
      # as no root (65532), adding only the capability to bind 80 and 443, and started as the image starts it: an exec
      # through sh would lose that capability. It starts only once its user can write /data, else says what to run.
      securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, fsGroup: 65532, fsGroupChangePolicy: OnRootMismatch, seccompProfile: {type: RuntimeDefault}}
      initContainers:
        - name: volume-owned
          image: docker.io/library/caddy:2@sha256:0c994536bddb66445885237f1a5dcc1916bccea922661c76b4e9fc24061f9b52
          command: [sh, -c, "d=/data; u=$(id -u); g=$(id -g); bad=$( { find \"$d\" -mindepth 1 -type d ! -user \"$u\" ! \\( -group \"$g\" -perm -070 \\); find \"$d\" -mindepth 1 ! -type d ! -user \"$u\" ! \\( -group \"$g\" -perm -060 \\); } | head -n 3 | tr '\\n' ' '); if [ -n \"$bad\" ] || ! touch \"$d/.canyon-writable\" || ! rm \"$d/.canyon-writable\"; then echo \"canyon: user $u can't write $d ($(echo ${bad:-$d})). Hand it to user $u as docs/self-host/backup-and-upgrade.md says (The upgrade that runs the chart's bundled parts as no root), then delete this pod.\" >&2; exit 1; fi"]
          securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
          volumeMounts: [{name: data, mountPath: /data}]
      containers:
        - name: caddy
          image: docker.io/library/caddy:2@sha256:0c994536bddb66445885237f1a5dcc1916bccea922661c76b4e9fc24061f9b52
          ports: [{containerPort: 80}, {containerPort: 443}]
          volumeMounts:
            - {name: config, mountPath: /etc/caddy/Caddyfile, subPath: Caddyfile}
            - {name: data, mountPath: /data}
            - {name: autosave, mountPath: /config}
          securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL], add: [NET_BIND_SERVICE]}}
      volumes:
        - {name: config, configMap: {name: caddy}}
        - {name: data, persistentVolumeClaim: {claimName: caddy-data}}
        - {name: autosave, emptyDir: {}}
---
apiVersion: v1
kind: Service
metadata: {name: caddy, namespace: canyon-gateway}
spec:
  type: LoadBalancer
  externalTrafficPolicy: Local
  selector: {app: caddy}
  ports: [{name: http, port: 80}, {name: https, port: 443}]
EOF
kubectl apply -f $WORK/gateway.yaml
kubectl -n canyon-gateway rollout status deploy/caddy --timeout=5m
```

`Recreate` stops the old pod before the new one starts, so the new one can take the same address.
Check the pod has it, then have Caddy issue the certificates of the two addresses the front-door
check dials:

```sh
kubectl -n canyon-gateway get pod -l app=caddy -o jsonpath='{.items[0].status.podIP}'   # $CADDY_IP
for env in preview prod; do curl -s -o /dev/null --max-time 60 https://canyon-probe.$env.$DOMAIN/; done
```

Issuing one takes a few seconds, longer than the check waits, so without the second line the first
save of the front door fails with "does not resolve or does not answer on 443".

After editing the Caddyfile, reapply it and run `kubectl -n canyon-gateway rollout restart deploy/caddy`.

**An OIDC issuer served through this Caddy** (Dex, Keycloak or Zitadel at `https://<issuer>.$DOMAIN`)
needs two more policies. Its name resolves to the node, and kube-proxy rewrites that address to
Caddy's pod address before policies match. The server's egress and the apps' egress admit public
addresses only, so they leave the pod address out. Without these policies the server never
completes OIDC discovery, so nobody can sign in, and every deploy fails with `container canyon-identity … restarted` and
the sidecar's timeout quoted in the server's log. Through Caddy, a pod reaches only what the
internet can. In this setup the server isn't ready until its issuer answers through Caddy, so step
6's wait for `canyon-server` times out. Go on, apply these policies, then wait for the server.

The apps' policy leaves out apps routed through Canyon's egress gateway (pods labelled `canyon.floor`),
whose way out is that gateway alone. They can't reach an issuer served through this Caddy: kube-proxy
turns the node's address into Caddy's pod address, which the egress gateway refuses. So tell Canyon:
add `CANYON_KUBE_GATEWAY_SIGNIN=https://<issuer>.$DOMAIN` to step 5's Secret `canyon-server`
(`kubectl -n canyon patch secret canyon-server -p '{"stringData":{"CANYON_KUBE_GATEWAY_SIGNIN":"https://<issuer>.'$DOMAIN'"}}'`,
then `kubectl -n canyon rollout restart deploy/canyon-server`). Canyon then refuses to route apps through
its egress gateway while the front door signs in there. Routed apps need a sign-in provider outside the cluster
([A private sign-in provider for apps routed through Canyon](/self-host/kubernetes/#a-private-sign-in-provider-for-apps-routed-through-canyon)),
or leave egress off.

```sh
cat > $WORK/issuer-policy.yaml <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: canyon-server-gateway, namespace: canyon}
spec:
  podSelector: {matchLabels: {app.kubernetes.io/name: canyon-server}}
  policyTypes: [Egress]
  egress:
    - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: canyon-gateway}}, podSelector: {matchLabels: {app: caddy}}}]
      ports: [{protocol: TCP, port: 443}]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: canyon-apps-gateway, namespace: canyon-apps}
spec:
  podSelector: {matchExpressions: [{key: canyon.floor, operator: DoesNotExist}]}
  policyTypes: [Egress]
  egress:
    - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: canyon-gateway}}, podSelector: {matchLabels: {app: caddy}}}]
      ports: [{protocol: TCP, port: 443}]
EOF
kubectl apply -f $WORK/issuer-policy.yaml
kubectl -n canyon rollout status deploy/canyon-server --timeout=10m
```

## 8. Verify

```sh
kubectl get pods -A | grep -E '^(canyon|kpack)'   # every pod Running and READY
kubectl wait clusterbuilder/canyon --for=condition=Ready --timeout=15m   # kpack builds the builder after the stack and store; minutes
kubectl -n canyon logs deploy/canyon-server | head -5   # "auth oidc", "oidc discovery … complete", "listening on [::]:7777"
curl -s -o /dev/null -w '%{http_code}\n' https://$DOMAIN/ready   # 200, over a Let's Encrypt certificate
```

If the ClusterBuilder wait times out, check `kubectl get clusterlifecycle default-lifecycle`. If
kpack first read it while DNS or egress was broken, it stays not ready with a `lookup … server
misbehaving` message after the network is fixed. `kubectl -n kpack rollout restart deploy/kpack-controller`
makes kpack read it again.

## 9. Use it

Sign in at `https://$DOMAIN` as one of `CANYON_ADMINS`. Then open **Settings › App hosting**,
enter your domain, choose the address form `app.env.domain` and how people sign in to apps, and save
(the install guide, *Last, set the front door*). Saving runs Canyon's NetworkPolicy and certificate check.

Then deploy a first app from any machine, with the CLI of the same release
([Deploy on your company's Canyon](/start/deploy-on-your-companys-canyon/) is the page to send your
developers):

```sh
curl -fsSL https://github.com/getcanyon-ai/canyon-releases/releases/download/$RELEASE/install.sh -o install.sh && sh install.sh
canyon login https://$DOMAIN    # opens a sign-in link
cd ~/my-app && canyon deploy    # reviews and builds it, then runs it at https://my-app.preview.$DOMAIN
canyon deploy --env prod        # ships what preview runs to https://my-app.prod.$DOMAIN
```

## Caveats

- **k3s's own ports are open to the internet.** k3s serves its API on 6443 and the kubelet on 10250
  on every interface, over IPv4 and IPv6. ServiceLB also allocates a NodePort for every LoadBalancer
  Service, which reaches the gateway around any filter on 80/443. Step 1's firewall closes all three.
- **Caddy's pod address is pinned** to `$CADDY_IP` (steps 5 and 7), and Calico reserves it. Give no
  other pod that address with the same annotation: whichever holds it is the one Canyon believes about
  who is asking.
- **The database CIDRs are the whole pod network**, which is safe only on a dedicated host (step 6).
- **The server's Postgres goes through a Service.** Point `egress[3]` and `egress[4]` at the pod
  network, as step 6 does. With the Service's ClusterIP there, the server can't connect.
