---
title: Install on Kubernetes
description: Install Canyon for production on your own cluster, with your registry, network plugin, gateway, identity provider and databases.
---

| | |
|---|---|
| **Time** | About an hour, once what you bring is ready |
| **You need** | Kubernetes 1.29 or newer, with a network plugin that enforces NetworkPolicy · kpack · an OCI registry · a gateway or ingress with a TLS certificate · an OIDC identity provider · Postgres for Canyon · a default StorageClass · CPU for the scans and apps · *optional:* Postgres for apps' databases. Details: [You bring](#you-bring) |
| **You get** | Canyon at `https://<domain>`, and apps at `https://<app>.preview.<domain>` and `https://<app>.prod.<domain>` (or `<app>-preview.<domain>`, the other address form), each behind sign-in |
| **Not covered** | Backups: Canyon backs up nothing ([Back up, restore and upgrade](/self-host/backup-and-upgrade/)) |

This is the production install. Canyon installs its own few components into your cluster and uses
your registry, network plugin, gateway and databases. It never creates clusters, networks,
certificates, database servers or cloud credentials.

The release's `deploy/k8s` directory (in its `canyon-deploy.tar.gz`) is the install base: a kustomize
base your overlay adds the values only you know to. For a complete worked example on one host,
with the registry, both databases and the gateway in the cluster, see
[One k3s host, step by step](/self-host/k3s/). To try Canyon on one node first, use
[the Helm chart](/self-host/helm/). It is not for production.

```diagram kind=layers title="Canyon on your cluster: Canyon's four namespaces, between what you bring"
layers:
  - band: You bring
    tiles:
      - {what: "Your gateway or ingress, with TLS", more: "Canyon's address and every app's address"}
      - {icon: key, who: Sign-in, what: Your identity provider}
  - via: "both addresses reach the namespace `canyon`"
  - band: "Canyon's namespaces"
    tone: canyon
    tiles:
      - {tone: canyon, what: "`canyon`", more: "`canyon-server`, the executor, `canyon-router`, the egress gateway"}
      - {tone: canyon, what: "`canyon-scans`", more: "scan pods, with nothing in or out but Trivy's server"}
      - {tone: canyon, what: "`canyon-builds`", more: "kpack builds, which push images to your registry"}
      - {tone: canyon, what: "`canyon-apps`", more: "each app's pod, with `canyon-identity` in front"}
  - via: the server, the builds and the apps reach
  - band: You bring
    tiles:
      - {what: "Postgres for Canyon"}
      - {who: Optional, what: "Postgres for apps", more: "port 5433, for apps that declare a database"}
      - {what: kpack, more: runs the builds}
      - {what: Your OCI registry}
```

The tables below list each part. [How Canyon works](/concepts/how-canyon-works/#what-runs-where-on-kubernetes)
shows who talks to whom, port by port.

## How people reach apps

The **front door** is how people get to an app: the domain apps live under, the address form, and
who signs people in. An admin sets it in **Settings › App hosting** once Canyon is up (the last step
of [Install](#install)). [How people reach apps](/self-host/front-door/) defines it and draws each
choice. To have your own deployment platform run the apps instead of Canyon (`run_by: org`), see
[Your platform runs the apps](/self-host/hand-off/).

## Install for production

1. **Bring what Canyon needs** (*You bring*): Kubernetes 1.29 or newer with a network plugin that
   enforces NetworkPolicy and cluster DNS labelled `k8s-app: kube-dns`, kpack, an OCI registry, a
   gateway with TLS, an OIDC identity provider, Postgres for Canyon, a default StorageClass, CPU for
   the scans and apps and, if apps declare Postgres, a Postgres server for them.
2. **Write one overlay** of this directory with the values only you know, after *Install*'s steps 1
   to 6 in order: Canyon's five images and its Opengrep and ESLint images by digest, the registry and
   its credential, the builder's tag, the database CIDRs and the Secrets `canyon-server` and
   `canyon-egress`. Apply it with `kubectl apply -k <your overlay>`.
3. **Only if apps' calls will go through Canyon and your sign-in provider isn't public:** list the
   provider's sign-in hosts, and, for a provider inside the cluster, admit routed pods to it
   ([A private sign-in provider for apps routed through Canyon](#a-private-sign-in-provider-for-apps-routed-through-canyon)).
4. **Upgrade** as [Back up, restore and upgrade](/self-host/backup-and-upgrade/#upgrade) says: it is
   the one place the steps are written.
5. **Back up both databases yourself**, Canyon's Postgres and the apps' Postgres, and the server's
   volume, which holds the key every Canyon secret is sealed under. Canyon backs up none of them.

## Canyon ships

| Component | What it does | Defined in |
|---|---|---|
| `canyon-server` | API and dashboard, the review, policies, approvals, audit, vault, runs, and the receiver of the egress gateway's records (port 7781, from the gateway's pods alone) | `server.yaml`: one replica, and a 10 Gi volume holding the key every secret is sealed under and the package store ([Limits](#limits)) |
| `canyon-executor` | Builds, runs, scans, app access and the cluster probe. Only the server's pods can reach it. | `executor.yaml` |
| `canyon-identity` sidecar | Runs beside every app and checks sign-in and team access on every request | Written into each app's pod by the executor, from `CANYON_IMAGE_IDENTITY` |
| oauth2-proxy | Bundled sidecar that signs people in when the front door is "Canyon signs people in" (**Settings › App hosting**) | Written into the app's pod by the executor |
| `canyon-router` | nginx that sends each app address to its app | `router.yaml` |
| `canyon-egress` | Canyon's egress gateway: Envoy, two replicas, the one way out of an app whose calls are routed through Canyon (`HTTPS_PROXY`), to public addresses on 80 and 443 alone ([Limits](#limits)) | `egress.yaml`, the stock `envoyproxy/envoy` on `deploy/egress`'s bootstrap |
| Install base | Four namespaces (`canyon`, `canyon-apps`, `canyon-builds`, `canyon-scans`), least-privilege roles, and NetworkPolicies for the server, the executor, the router, the egress gateway and the build pods | `namespaces.yaml`, `rbac.yaml`, `server.yaml`, `executor.yaml`, `router.yaml`, `egress.yaml`, `builds.yaml` |
| Builder and scanners | The kpack ClusterStack, ClusterStore and ClusterBuilder `canyon` (Paketo on Ubuntu Noble); Trivy, Grype and Checkov; Opengrep and ESLint, from the release's `canyon-opengrep` and `canyon-eslint` images | `builds.yaml`; each scan is a pod the executor starts |

Each app image's package list (a CycloneDX SBOM) is published beside the image in your registry, as an
OCI 1.1 referrer your own scanners find by the image's digest. One per image, removed with the image. A
registry without the Referrers API gets none, and a failed publish never fails the scan.

Every image is pinned by digest. The third-party images (Paketo, oauth2-proxy, nginx, Envoy, busybox and the
scanners) are pinned in these files and in the executor, and your nodes pull them from docker.io,
ghcr.io and quay.io. To pull the executor's from a mirror instead, set its `CANYON_IMAGE_*` variables
(*Install*); Paketo's, nginx's and Envoy's are in these files, which your overlay patches.

Canyon's own seven images are published with every release, each for both `linux/amd64` and
`linux/arm64`, so a node pool of either architecture, or both, pulls the same digest:
`canyon-server`, `canyon-executor-kube`, `canyon-identity`, `canyon-run`, the image every app runs
on: Paketo's noble run image with `libatomic1`, which Node 25 and later need, `libsqlite3-0`, which
Python's `sqlite3` module links, and `libpq5`, which PHP's Postgres extensions link,
`canyon-build`, the image every app builds on: Paketo's noble build image with the same `libpq5`, and the scanner images `canyon-opengrep` and `canyon-eslint`. The release's `images.txt`
names each one by digest, beside the laptop demo's `canyon-executor` and `canyon-demo-apps`, which a
cluster doesn't need. Before a release is published, its workflow pulls every one of them by
that digest on both architectures. A release's nine, their digests elided here:

```text
ghcr.io/getcanyon-ai/canyon-build@sha256:<digest>
ghcr.io/getcanyon-ai/canyon-demo-apps@sha256:<digest>
ghcr.io/getcanyon-ai/canyon-eslint@sha256:<digest>
ghcr.io/getcanyon-ai/canyon-executor@sha256:<digest>
ghcr.io/getcanyon-ai/canyon-executor-kube@sha256:<digest>
ghcr.io/getcanyon-ai/canyon-identity@sha256:<digest>
ghcr.io/getcanyon-ai/canyon-opengrep@sha256:<digest>
ghcr.io/getcanyon-ai/canyon-run@sha256:<digest>
ghcr.io/getcanyon-ai/canyon-server@sha256:<digest>
```

They are public, and pull without a login. Copy them into your registry and name each one in your
overlay by the same digest. `latest/download` takes the newest release; for another, put
`download/<release>` (such as `download/v0.2.3`) in its place, here and below, and tag the copies
with that release. On ECR,
`canyon-run` and `canyon-build` must be there: kpack pulls them with the executor's ECR credential.
[crane](https://github.com/google/go-containerregistry/tree/main/cmd/crane) copies an image with
its digest unchanged:

```sh
curl -fsSL -o images.txt https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/images.txt
while read -r ref; do name=${ref##*/}; crane copy "$ref" "<registry host>/${name%@*}:<release>"; done < images.txt
```

### Which architecture apps run on

Canyon's own images run on either architecture, but each app image Canyon builds is made for one. kpack
makes its builder for the architecture of the node its controller runs on, pins each build's pod to that
architecture, and the build makes an image of it.

Each deploy reads its image's architecture from your registry and pins the app's pods to nodes of it
(`kubernetes.io/arch`), so a cluster with both arm64 and amd64 nodes runs each image where it can start. That
holds for a promotion, a rollback and a patch too, whichever architecture the image was built on. A deploy
whose image's architecture can't be read runs nowhere and changes nothing: `the image <image>'s architecture
could not be read, so Canyon runs it nowhere`. After an upgrade, each app keeps running as it is, and takes the pin
the next time it is deployed, which rolls it out once.

To build every app for one architecture, pin kpack's `kpack-controller` to its nodes with a `nodeSelector` in
your overlay. Under hand-off, your platform runs the image on its own nodes: each release names the image's
`platform`, and the deploy repo's Deployment pins it to nodes of that architecture
([Hand-off](/self-host/hand-off/)). Canyon's CI builds and runs an app this way on arm64 and on amd64.

## You bring

| Requirement | Why Canyon needs it | How Canyon checks it | Examples |
|---|---|---|---|
| Kubernetes 1.29 or newer | An app's pod runs `canyon-identity` (and, where Canyon signs people in, oauth2-proxy) as a native sidecar: an init container with `restartPolicy: Always`. Versions before 1.29 silently drop that field, so the app never starts. | Documented only | Tested on EKS and single-node k3s; kind in CI (1.34 and 1.30) |
| A network plugin that enforces NetworkPolicy, from a pod's first packet | Isolation is enforced by NetworkPolicy. Apps can't reach each other, Canyon's internals, or an app's port around its sign-in. The executor's own policy is the only gate in front of it. | A live probe on every save of the front door (**Settings › App hosting**) refuses and names the switch to turn on, for a plugin that ignores policy or lets a new pod out before its policy is in force. The one exception is hand-off: with your platform's webhook saved, a "Your sign-in proxy signs people in" door that fails is saved *not checked on this cluster*, and Canyon runs no app behind it ([Your platform runs the apps](/self-host/hand-off/)). Either way, Canyon builds nothing on a cluster the check hasn't seen enforce NetworkPolicy: every deploy fails before the build until it does. When an app can reach the server it runs on, the save goes through with a warning (*Host firewall*, below). CI proves both answers: `enforced` on kind with Calico, `not_enforced` on kind v0.23's kindnet. | Calico, Cilium, AWS VPC CNI with `enableNetworkPolicy` `"true"` and `NETWORK_POLICY_ENFORCING_MODE` `strict`. Strict mode gives a pod that no NetworkPolicy selects no traffic at all, the cluster's own add-ons included: before you install Canyon, give `kube-system` and kpack's namespace policies of their own (*Strict mode on the AWS VPC CNI*, below). Canyon's four namespaces ship theirs. |
| Cluster DNS pods labelled `k8s-app: kube-dns` in `kube-system` | Every Canyon NetworkPolicy lets DNS out to those pods and no others. | Documented only | CoreDNS or kube-dns as EKS, k3s and kind install them |
| kpack | Builds each app's code into an image with Cloud Native Buildpacks, from the ClusterBuilder `canyon` | The executor waits for kpack's Builds at start and stays not ready until it can watch them. A build fails with "the ClusterBuilder canyon has no image yet" until kpack has built the builder. | kpack v0.18.0 (what CI runs), with the base's ClusterBuilder `canyon` or your own under that name |
| An OCI registry | Holds each app's images, its build cache, the source image each build starts from, the builder and `canyon-run`. Your nodes pull from it with their own credential: Canyon writes no `imagePullSecrets`. | On ECR the executor refuses to start without its own AWS credential and never falls back to the node's. | ECR, with an IAM role for the executor (IRSA). Any other registry must, for now, accept the executor's pushes, tag lists and deletes without a credential. kpack pushes with `canyon-registry`, which the executor writes on ECR and you create elsewhere (*Install*, step 3). |
| An ingress or gateway, and a TLS certificate | Sends `*.preview.<domain>` and `*.prod.<domain>` (form `app.env.domain`) or `*.<domain>` (form `app-env.domain`) to `canyon-router`, and Canyon's own address to `canyon-server` | A TLS handshake on every save of the front door (**Settings › App hosting**) names the address that does not resolve, does not answer on 443, or has a certificate that does not cover it. | AWS ALB, NGINX Ingress, Envoy Gateway, Istio. `canyon logs -f` holds a request open while the app is quiet, and a gateway ends it at its idle timeout: an ALB's is 60 s by default (`alb.ingress.kubernetes.io/load-balancer-attributes: idle_timeout.timeout_seconds=3600`), NGINX's `proxy_read_timeout` 60 s. |
| An OIDC identity provider | People sign in to Canyon. For apps, either your gateway passes its sign-in token ("Your sign-in proxy signs people in") or the bundled oauth2-proxy uses an OIDC client of yours ("Canyon signs people in"). | Canyon refuses to start without its issuer, client and admins. The sidecar verifies the token on every app request. | Entra ID, Okta, Zitadel, Google Workspace; AWS ALB sign-in. Group grants read the groups claim (Settings, or `CANYON_GROUPS_CLAIM`) as an array of names or an object whose keys are the names: on Zitadel, its project roles, `urn:zitadel:iam:org:project:roles` |
| Postgres for Canyon | Canyon's own records, runs and vault | Canyon refuses to start without `CANYON_DATABASE_URL`. | Amazon RDS, or any Postgres |
| CPU for Canyon, its scans and the apps | Canyon's own pods request 660m CPU and 1,152 Mi: the server 250m, the executor 100m, two routers 50m each, two egress gateways 100m and 128 Mi each, and trivy's server 10m. Each scan runs as a pod that requests 260m (a trivy scan 60m) and 528 Mi while it runs, up to `CANYON_SCAN_CONCURRENCY` at once. A scan pod that waits for a node counts toward its 10-minute limit. Each running app requests `CANYON_KUBE_APP_CPU_REQUEST` and `CANYON_KUBE_APP_MEMORY_REQUEST`, 10m and 256 Mi unset (measured: 53 idle apps used 1.2m and 136 Mi at their 99th percentile), plus its sidecar's 10m. Apps under real load should set a higher CPU request, so the scheduler places them where they get it. | Not checked. A scan whose pod cannot schedule by its deadline writes "the cluster had no room to run it", quoting the scheduler, and the run fails before the review: nothing is judged, so there is nothing to fix or approve. The same deploy goes through once the cluster has room: add nodes or lower app requests. | A 2-vCPU node runs Canyon beside the cluster's own add-ons with room for one scan at a time; the base's 10 scans at once need about 2.6 vCPU more, across your nodes; at the defaults, each 100 running apps take about 2 vCPU and 28 GiB of requests (the app's and its sidecar's), so memory, not CPU, then decides how many fit |
| A default StorageClass | The server's 10 Gi volume, and a 5 Gi volume for each scanner database: the executor makes it at that tool's first scan, and refreshes it when the executor starts and every 6 hours | Not checked. Without one, the claims stay `Pending` and so do the pods that need them. | Your cluster's default class |
| Databases for apps *(optional)* | Apps that declare Postgres get their own database and login. They reach it on a Postgres server of its own that listens on 5433 inside `CANYON_KUBE_DB_CIDRS`, the only private address an app may reach. | A deploy that declares Postgres without `CANYON_APP_DATABASE_URL` fails and names the setting. | An admin login for that server, as `CANYON_APP_DATABASE_URL`; or your own driver ([the provisioner contract](/reference/contracts/provisioner/)) |

## Canyon never touches

| Yours | What Canyon does instead |
|---|---|
| Clusters and node pools | Runs in the cluster you give it |
| Networks, VPCs, DNS | Writes NetworkPolicies in its own namespaces and nothing else of the network |
| Certificate issuance | Checks your certificate when the front door is saved (**Settings › App hosting**); never issues one |
| Database servers | Creates databases and logins on the apps database server you name; never the server |
| Cloud IAM and credentials | On ECR, uses the role you give the executor to create and delete repositories under `canyon/`; creates no role or credential |
| Where `canyon-server` may connect | Under `run_by: org` it sends the release webhook to the address your admin names, a private one too, and an admin's **Send a test delivery** posts there whenever they ask, answering with the HTTP status it got: so the server answers an admin whether any `https` address they save answers, and how. Restrict the server's egress with your NetworkPolicy, which is the control on both, and open your pipeline's address in it if the pipeline is internal. |

Canyon writes Kubernetes objects in its four namespaces alone, plus the cluster-scoped objects of this
base: the namespaces, the executor's ClusterRole and its binding, and the kpack ClusterStack,
ClusterStore and ClusterBuilder. So whoever applies the overlay needs cluster-admin, as installing
kpack does.

## What you can swap

Each row is a seam in Canyon's contracts. Choosing a different option changes no Canyon code.

| Seam | Options | State |
|---|---|---|
| Where apps run | Docker host · Kubernetes · your own executor, for example on ECS or Nomad (ask us for the contract) | Pluggable today |
| Who signs people in to apps | Your gateway's OIDC token (`oidc`) · AWS load balancer sign-in (`alb`) · Canyon signs people in, with the bundled oauth2-proxy · Canyon's own sign-in (Docker only) | Pluggable today |
| App databases | Canyon's built-in driver on your Postgres · your own driver: Crossplane, Terraform, an internal API ([the provisioner contract](/reference/contracts/provisioner/)) | Pluggable today |
| Registry | ECR (automatic) · any OCI registry that accepts the executor's pushes without a credential | Pluggable today. A credential for the executor on another registry is planned. |
| Security scanning | Trivy, Grype, Checkov, Opengrep and ESLint, run by Canyon · imported SARIF reports · GitHub code scanning | Pluggable today |
| Builds | kpack · planned: a plain buildpacks Job, BuildKit or Tekton, or your CI pushes and Canyon verifies | Planned, when a customer needs it |

## Install

Download the release's `canyon-deploy.tar.gz` into the directory that will hold your overlay. It unpacks to
`canyon/`, and `canyon/deploy/k8s` is the install base every step below uses:

```sh
curl -fsSL https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/canyon-deploy.tar.gz | tar -xz
```

Then do these, in this order:

1. **kpack.** The base's ClusterStack, ClusterStore and ClusterBuilder need kpack's CRDs and webhook.
   The release's own objects need them too, so apply it twice:

   ```sh
   kubectl apply -f https://github.com/buildpacks-community/kpack/releases/download/v0.18.0/release-0.18.0.yaml
   kubectl wait --for=condition=Established crd --all
   kubectl -n kpack rollout status deploy/kpack-webhook
   kubectl apply -f https://github.com/buildpacks-community/kpack/releases/download/v0.18.0/release-0.18.0.yaml
   ```

   On the AWS VPC CNI in strict mode, give `kpack` its policy (*Strict mode on the AWS VPC CNI*,
   below) after the first apply, or its webhook never becomes ready.

2. **The builder's repository.** The base ships the store (Paketo, pinned by digest), which kpack's
   controller pulls from docker.io; the stack's build image is `canyon-build` and its run image
   `canyon-run`, which you push and your overlay names. kpack pushes the builder to `<registry>/canyon-builder`,
   which your overlay names. On ECR, create that repository yourself: the executor creates only
   `canyon/<app>`. Keep Canyon's own images outside `<registry>/canyon/` as well. Repositories there
   belong to apps, and forgetting an app deletes its repository.

3. **The registry credential.**
   - **ECR:** give the service account `canyon-executor` in `canyon` an IAM role through IRSA (the
     overlay's annotation below), holding exactly this policy: it is what bounds the executor's repository deletes. The executor writes
     `canyon-registry` from that role when it starts and every 6 hours after (a token lasts 12), so
     don't create it yourself. This is the policy our EKS test cluster runs, with its account and region
     replaced. If you push `canyon-run` and `canyon-build` to other repositories, name those instead:

     ```json
     {
       "Version": "2012-10-17",
       "Statement": [
         {
           "Sid": "ECRAuthToken",
           "Effect": "Allow",
           "Action": ["ecr:GetAuthorizationToken"],
           "Resource": "*"
         },
         {
           "Sid": "AppRepos",
           "Effect": "Allow",
           "Action": [
             "ecr:CreateRepository", "ecr:DeleteRepository", "ecr:BatchDeleteImage", "ecr:ListImages",
             "ecr:BatchCheckLayerAvailability", "ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage",
             "ecr:PutImage", "ecr:InitiateLayerUpload", "ecr:UploadLayerPart", "ecr:CompleteLayerUpload"
           ],
           "Resource": "arn:aws:ecr:<region>:<account>:repository/canyon/*"
         },
         {
           "Sid": "BuilderRepoPush",
           "Effect": "Allow",
           "Action": [
             "ecr:BatchCheckLayerAvailability", "ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage",
             "ecr:PutImage", "ecr:InitiateLayerUpload", "ecr:UploadLayerPart", "ecr:CompleteLayerUpload"
           ],
           "Resource": "arn:aws:ecr:<region>:<account>:repository/canyon-builder"
         },
         {
           "Sid": "StackImagesPull",
           "Effect": "Allow",
           "Action": ["ecr:BatchCheckLayerAvailability", "ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage"],
           "Resource": [
             "arn:aws:ecr:<region>:<account>:repository/canyon-run",
             "arn:aws:ecr:<region>:<account>:repository/canyon-build"
           ]
         }
       ]
     }
     ```

     The role's trust policy admits the cluster's OIDC provider for
     `system:serviceaccount:canyon:canyon-executor` alone.
   - **Any other registry:** create `canyon-registry`, the docker-config Secret kpack pushes with, in
     `canyon-builds`:

     ```sh
     kubectl apply -f canyon/deploy/k8s/namespaces.yaml
     kubectl -n canyon-builds create secret docker-registry canyon-registry \
       --docker-server=<registry host> --docker-username=<user> --docker-password=<token>
     ```

     The executor has no credential for such a registry. It pushes each build's source image, lists
     tags and deletes images anonymously, so the registry must allow that from the executor's pod.
     A registry at an RFC 1918 address is spoken to over plain http (the executor and kpack both
     do this), and your nodes' container runtime must be configured to pull from it the same way:
     with containerd, a `hosts.toml` for the registry's host that names its `http://` address.
   - kpack's build pods push to the registry, and the base's `canyon-builds` policy lets them out
     to public addresses on 80 and 443 alone: never the nodes' instance metadata address, Azure's
     WireServer or any private address, the same list an app's own policy leaves out. That holds only
     where the cluster enforces NetworkPolicy, so Canyon builds nothing on a cluster the front door's
     check hasn't seen enforce it, whoever runs the apps: on EKS, turn enforcement on in the VPC CNI
     add-on (*Strict mode on the AWS VPC CNI*, below), and give the nodes IMDSv2 with a hop limit of 1
     as well, so a pod doesn't reach the node's role even where a policy is missing. Canyon's own tests prove the policy
     on Calico, not on your cluster's network plugin. A registry at a private address, including one inside
     the cluster, needs an egress NetworkPolicy for `canyon-builds` in your overlay as well. Without
     it every build fails at `prepare` with `connect: connection refused`.
   - The executor's policy lets it out on TCP 443 alone, besides DNS. If your registry or your API
     server's endpoint listens on another port (a registry on 5000, kubeadm's 6443), add an egress
     NetworkPolicy for the executor in your overlay.

4. **The apps database** *(optional)*: a Postgres server of its own, listening on 5433 at an address
   inside `CANYON_KUBE_DB_CIDRS`. Nothing else in those ranges may listen on 5433, and Canyon's own
   Postgres stays on 5432. Every other private address is out of an app's reach, and out of its egress
   gateway's: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, `169.254.0.0/16`,
   `0.0.0.0/8`, `192.0.0.0/24`, `198.18.0.0/15`, `224.0.0.0/4`, `240.0.0.0/4` and `168.63.129.16`. That
   assumes your cluster's pod and Service addresses are among them. A cluster whose pods or Services
   use another range leaves them open to apps on 80 and 443 until Canyon takes your cluster's ranges as
   a setting. If it speaks TLS alone, as RDS does, apps need its certificate's CA to
   verify it: put the CA bundle (for RDS, Amazon's regional or global bundle) in a ConfigMap in
   `canyon`, `kubectl -n canyon create configmap apps-db-ca --from-file=ca.pem=<bundle>`, mount it
   into the server (the overlay's `canyon-server` patch below) and set `CANYON_APP_DATABASE_CA_FILE`.
   Its admin login, unless a superuser, needs `temp_file_limit` capped for the logins Canyon issues to
   people: have a superuser run `GRANT SET ON PARAMETER temp_file_limit TO <the admin login>` (Postgres
   15 or later), or set `temp_file_limit` to `1GB` or less for the whole server — on RDS, which has no
   superuser to grant it, in the instance's parameter group.

5. **The server's Postgres.** `CANYON_DATABASE_URL`'s address depends on your install, so the
   server's NetworkPolicy ships an egress rule to the placeholder `0.0.0.0/32` on 5432 — patch it to
   your Postgres's CIDR in your overlay, the way `CANYON_KUBE_DB_CIDRS` above patches the executor's.
   The server also dials the apps database directly on 5433 when `CANYON_APP_DATABASE_URL` is set
   (the built-in provisioner driver, step 4 above): a second placeholder egress rule patches to that
   CIDR the same way. The server also reads your cluster's service-account issuer and key set from the
   API server, to check the egress gateway's token: a third placeholder rule, on 443 and 6443, patches to
   the API server's endpoint addresses (`kubectl get endpoints kubernetes`), and the overlay adds the
   `kubernetes` Service's cluster IP to it (`kubectl get svc kubernetes -o jsonpath='{.spec.clusterIP}'`).
   It needs both: some CNIs match a pod's packet before kube-proxy rewrites the cluster IP to an endpoint
   (the VPC CNI's network policy, which EKS uses), others after. With the endpoints alone on EKS, the
   server's call to `https://kubernetes.default.svc` times out, the egress check holds no policy and
   the gateway's rollout never finishes. On EKS the endpoints move when the control plane is upgraded, so
   name the cluster's subnets, which hold them, rather than each endpoint's `/32`: the first in place of
   the placeholder, each other one added the way the cluster IP is. **The three are fixed at
   `egress[3]` (5432), `egress[4]` (5433) and `egress[5]` (the API server) in
   `canyon/deploy/k8s/server.yaml` — their index is load-bearing.** The egress gateway's NetworkPolicy
   likewise admits the egress check's Prometheus metrics (port 7784, `GET /metrics`) from a placeholder
   namespace no namespace is named, `ingress[2]` in `canyon/deploy/k8s/egress.yaml`: patch in your scraper's
   namespace to scrape them, or leave it and nothing reaches them. A
   sign-in provider on a private range (an internal IdP) is likewise the overlay's to add — patch in
   its CIDR on 443 the same way you would Postgres's. If a database runs in the cluster behind a
   Service, patch in the narrowest range holding the database's pods. If your CNI matches before the
   Service rewrite (as EKS's VPC CNI does), a database reached through a Service needs its cluster IP
   too. The same applies to `CANYON_KUBE_DB_CIDRS`.

   Apps reach the sign-in provider too, from their own pods. Where Canyon signs people in, the sidecars fetch its
   discovery document and key set at start, and oauth2-proxy calls it at every sign-in. Under choice
   1 `oidc` the sidecar fetches the gateway's key set. The policy the executor writes for each app
   lets it out to public addresses alone. So an internal IdP needs an egress NetworkPolicy in
   `canyon-apps` as well, selecting every pod (`podSelector: {}`), to its CIDR on 443. The same holds
   when the issuer is served through this cluster's own load balancer, on any cluster whose
   LoadBalancer Services publish an IP (MetalLB, k3s's ServiceLB). kube-proxy rewrites that
   IP to the gateway pod's before policies match, so both policies, the server's and the apps', name
   the gateway's pods by namespace and label, as [the k3s guide's step 7](/self-host/k3s/#7-the-gateway) does. Without them nobody can sign
   in, and every deploy fails with the sidecar's timeout quoted in the server's log. These policies are for
   apps that call out directly. An app routed through Canyon reaches out through its egress gateway alone,
   which refuses private addresses; it reaches an internal IdP only as the next paragraph says. So leave
   its pods, labelled `canyon.floor`, out of an IdP policy by address
   (`podSelector: {matchExpressions: [{key: canyon.floor, operator: DoesNotExist}]}`). A deploy that
   first routes an app through Canyon labels only its new pods: the pods it replaces keep what such a
   policy lets out (on k3s, the chart's `canyon-apps-gateway` to the gateway on 443) until the rollout
   has replaced them.

   An internal IdP for apps routed through Canyon needs more: see
   [A private sign-in provider for apps routed through Canyon](#a-private-sign-in-provider-for-apps-routed-through-canyon).

6. **Build values in `canyon-builds`.** An app's build values are plain `spec.env` on its kpack Build
   and its build pod, not in a Secret (kpack refuses a Secret reference there). Bound who reads them,
   encrypt them at rest and keep them out of audit request bodies. A crashed app's last log lines sit in its pod's status in
   `canyon-apps` the same way, so the same three controls cover `pods` there.

Your overlay's `resources` names `canyon/deploy/k8s`, from the `canyon-deploy.tar.gz` you downloaded at the
top of *Install*. Keep your overlay in a directory beside `canyon/`.

Then write your overlay:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../canyon/deploy/k8s # deploy/k8s from the canyon-deploy.tar.gz of the release you install
images:
  - {name: canyon-server, newName: <registry host>/canyon-server, digest: "sha256:<digest>"}
  - {name: canyon-executor-kube, newName: <registry host>/canyon-executor-kube, digest: "sha256:<digest>"}
patches:
  - target: {kind: Deployment, name: canyon-server} # only with CANYON_APP_DATABASE_CA_FILE (step 4)
    patch: |-
      [{"op": "add", "path": "/spec/template/spec/volumes/-", "value": {"name": "apps-db-ca", "configMap": {"name": "apps-db-ca"}}},
       {"op": "add", "path": "/spec/template/spec/containers/0/volumeMounts/-", "value": {"name": "apps-db-ca", "mountPath": "/etc/apps-db-ca", "readOnly": true}}]
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
                  - {name: CANYON_IMAGE_IDENTITY, value: "<registry host>/canyon-identity@sha256:<digest>"}
  - target: {kind: Deployment, name: canyon-executor} # a merge by name: every variable the base sets stays
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata: {name: canyon-executor, namespace: canyon}
      spec:
        template:
          spec:
            containers:
              - name: executor
                env:
                  - {name: CANYON_KUBE_REGISTRY, value: "<registry host>"}
                  - {name: CANYON_IMAGE_IDENTITY, value: "<registry host>/canyon-identity@sha256:<digest>"}
                  - {name: CANYON_IMAGE_OPENGREP, value: "<registry host>/canyon-opengrep@sha256:<digest>"}
                  - {name: CANYON_IMAGE_ESLINT, value: "<registry host>/canyon-eslint@sha256:<digest>"}
                  - {name: CANYON_KUBE_DB_CIDRS, value: "<cidr>,<cidr>"}
  - target: {kind: ClusterBuilder, name: canyon}
    patch: '[{"op": "replace", "path": "/spec/tag", "value": "<registry host>/canyon-builder"}]'
  - target: {kind: ClusterStack, name: canyon}
    patch: '[{"op": "replace", "path": "/spec/runImage/image", "value": "<registry host>/canyon-run@sha256:<digest>"}, {"op": "replace", "path": "/spec/buildImage/image", "value": "<registry host>/canyon-build@sha256:<digest>"}]'
  - target: {kind: ServiceAccount, name: canyon-executor} # ECR only
    patch: '[{"op": "add", "path": "/metadata/annotations", "value": {"eks.amazonaws.com/role-arn": "arn:aws:iam::<account>:role/<role>"}}]'
  - target: {kind: NetworkPolicy, name: canyon-server}
    patch: |-
      [{"op": "replace", "path": "/spec/egress/3/to/0/ipBlock/cidr", "value": "<your Postgres's CIDR>"},
       {"op": "replace", "path": "/spec/egress/4/to/0/ipBlock/cidr", "value": "<the apps database's CIDR (delete this line if you don't set CANYON_APP_DATABASE_URL)>"},
       {"op": "replace", "path": "/spec/egress/5/to/0/ipBlock/cidr", "value": "<the API server's endpoint addresses' CIDR; on EKS, a subnet of the cluster's>"},
       {"op": "add", "path": "/spec/egress/5/to/-", "value": {"ipBlock": {"cidr": "<the kubernetes Service's cluster IP>/32"}}}]
  - target: {kind: NetworkPolicy, name: canyon-egress} # if you scrape the egress check's metrics
    patch: '[{"op": "replace", "path": "/spec/ingress/2/from/0/namespaceSelector/matchLabels/kubernetes.io~1metadata.name", "value": "<the namespace of your scraper>"}]'
```

| Executor variable | Value |
|---|---|
| `CANYON_KUBE_REGISTRY` | The registry host every image lives under. ECR's form, `<account>.dkr.ecr.<region>.amazonaws.com`, turns on ECR. |
| `CANYON_IMAGE_IDENTITY` | `canyon-identity`, by digest |
| `CANYON_IMAGE_OPENGREP` | Canyon's Opengrep image, `canyon-opengrep` from the release's `images.txt`, copied to your registry: `<registry>/canyon-opengrep@sha256:<digest>`. Unset, an Opengrep scan answers `no_image`, and a review with Opengrep on blocks on it as a scan that could not check ([Canyon-run tools](/reference/contracts/finding/#canyon-run-tools)). |
| `CANYON_IMAGE_ESLINT` | Canyon's ESLint image, `canyon-eslint` from `images.txt`, the same way: `<registry>/canyon-eslint@sha256:<digest>`. Unset, an ESLint scan answers `no_image`, and a review with ESLint on blocks on it. |
| `CANYON_KUBE_DB_CIDRS` | The CIDRs, comma-separated, where the apps database listens on 5433. Empty, no app reaches a database. |
| `CANYON_IMAGE_OAUTH2_PROXY`, `CANYON_IMAGE_BUSYBOX`, `CANYON_IMAGE_BUSYBOX_STATIC`, `CANYON_IMAGE_TRIVY`, `CANYON_IMAGE_GRYPE`, `CANYON_IMAGE_CHECKOV` | Optional: where each of those images comes from, a copy in your registry by digest (`<registry>/<repository>@sha256:<hex>`). Unset, the image pinned in this release. |
| `CANYON_SCANDB_TRIVY` | Optional: the OCI repository and tag Trivy's database downloads from, e.g. `<registry host>/aquasec/trivy-db:2`, by Trivy's server `canyon-trivy`, which every Trivy scan is a client of. Unset, Trivy's own public sources. |
| `CANYON_SCANDB_GRYPE` | Optional: the `https` URL Grype's database downloads from, serving `v6/latest.json` and its archives, with no credential in it. Unset, `https://grype.anchore.io/databases`. |
| `CANYON_SCANDB_TRIVY_JAVA` | Optional: the OCI repository and tag Trivy's Java database downloads from, e.g. `<registry host>/aquasec/trivy-java-db:1`, into the 5 Gi `ReadWriteOnce` volume `canyon-scandb-trivy-java`, which image scans read one at a time. Unset, Trivy's own public sources. An air-gapped cluster mirrors it as it mirrors Trivy's main database. |
| `CANYON_SCAN_IMAGE_MAX` | Optional: the largest image an image scan reads, in the compressed bytes its manifest names; a whole number of bytes, or of `Ki`, `Mi`, `Gi` or `Ti`. Default `8Gi`. The scan pod requests about twice it in ephemeral storage, so keep it at or below half the free ephemeral storage of the smallest node that runs scans; a larger image waits for room and then reads as not checked. A 460 MB image scans in about 35 seconds after its deploy. |
| `CANYON_EXECUTOR_BUILDS` · `CANYON_EXECUTOR_SCANS` | Optional: the builds and scans the executor takes at once, 6 and 14 unset. Keep them at or above the server's `CANYON_BUILD_CONCURRENCY` plus `CANYON_PATCH_CONCURRENCY`, and its `CANYON_SCAN_CONCURRENCY` plus `CANYON_PATCH_CONCURRENCY` plus two, so a busy answer means real overload. |
| `CANYON_KUBE_APP_CPU_REQUEST` · `CANYON_KUBE_APP_MEMORY_REQUEST` | Optional: what each app container requests, 10m and 256Mi unset; a Kubernetes quantity above 0 and at most the app's limit (1 CPU, 1Gi), else the executor does not start. Raise them for apps under real load. |
| `CANYON_KUBE_SCANDB_CIDRS` | Optional: the CIDRs, comma-separated, of a database source that is not a public address on 443. A refresh reaches every TCP port there, so none may overlap `169.254.0.0/16`, the node's instance metadata. |
| `CANYON_KUBE_SIGNIN_DIRECT` | Optional: the sign-in hosts that apps routed through Canyon's egress gateway call directly, comma-separated, each `<host>=<address>/32` outside the cluster or `<host>` inside it. An empty entry, a range, an address without `/32`, IPv6 or link-local stops the executor ([A private sign-in provider for apps routed through Canyon](#a-private-sign-in-provider-for-apps-routed-through-canyon)). Those calls aren't recorded. |

If a required variable is left empty, or any variable is not what its row says, the executor refuses
to start and names it.

The server reads the Secret `canyon-server` in `canyon`. Create it the way your estate creates
Secrets, for example `kubectl -n canyon create secret generic canyon-server --from-env-file=<file>`:

| Key | Value |
|---|---|
| `CANYON_DATABASE_URL` | Canyon's Postgres. Required. Set `sslmode=verify-full` with `sslrootcert`, or `sslmode=require` at the least: [Configuration](/self-host/configuration/) says what each does. |
| `CANYON_DATABASE_MAX_CONNS` | Optional: the most connections the server opens to its Postgres, 20 unset, 4 to 500. It also opens one more for its readiness probe, and its run driver its own few: keep this under the database's connection limit. |
| `CANYON_PUBLIC_URL` | `https://<host>`: where people open Canyon, and the issuer of the tokens apps receive. Required: unset, or `http` on a host other than loopback, Canyon refuses to start. |
| `CANYON_OIDC_ISSUER` | Canyon's sign-in issuer, an `https` URL. Required. |
| `CANYON_OIDC_CLIENT_ID` | Its client, with the redirect URI `<CANYON_PUBLIC_URL>/auth/callback`. Canyon asks for the scopes `openid profile email`. Required. |
| `CANYON_OIDC_CLIENT_SECRET` | If the client has one. Without it, Canyon signs in as a public PKCE client. |
| `CANYON_ADMINS` | Comma-separated emails of the first admins, the only people who can sign in until an admin finishes setup. Required. |
| `CANYON_OIDC_ALLOWED_DOMAINS` · `CANYON_OIDC_ALLOWED_EMAILS` | Optional: who else may sign in — anyone with an email at one of these domains, or one of these addresses, comma-separated. Unset, the first admin to sign in sets them on the setup screen. Set in the environment, a list is fixed and the setup screen shows it read-only. With both set, Canyon refuses to start unless every `CANYON_ADMINS` address is on one of them; otherwise setup can't finish until at least one admin is on either list. |
| `CANYON_IMAGE_IDENTITY` | Not in this Secret: `server.yaml` sets it empty in the server's own env, so set it with the overlay's `canyon-server` patch above, to the executor's value. Every release to your platform names it as the sign-in sidecar, so while it is empty an admin can't hand apps off to your platform, and a server that already did refuses to start, naming it. Hand-off also needs `CANYON_PUBLIC_URL` to be `https`. |
| `CANYON_PATCHING` | **Beta**, not in this Secret: `server.yaml` sets it `off` in the server's own env, which wins over a value here, so turn it on by patching the `canyon-server` Deployment's env in your overlay to `on`. Turning it on lets Canyon rebase an app's preview onto a newer run image and redeploy it on its own, then watch it for `CANYON_PATCH_WATCH`: if the patched app exits or restarts, Canyon puts back the image it replaced. Canyon patches one app's preview for each new run image first, and more previews in waves once a watch passes. It never patches production on its own: once a patched preview's watch passes, Canyon asks for production with a production approval, which a contributor or above on the app approves in the dashboard, and asks again each rescan while the patch still applies. Once approved, production is watched for `CANYON_PATCH_WATCH` too, and a crash there puts back the image production ran. Off, Canyon still checks the packages it put in the images of apps where `platform.known-vuln` is on, and no patch runs. Any value but `on` or `off` stops the server from starting. |
| `CANYON_SCAN_CONCURRENCY` · `CANYON_BUILD_CONCURRENCY` · `CANYON_PATCH_CONCURRENCY` | Not in this Secret: `server.yaml` sets them to 10, 4 and 2 in the server's own env, which wins over a value here, so raise them by patching the `canyon-server` Deployment's env in your overlay. They are the scans, builds and platform patches the host runs at once, across every run (patches are **beta**, and run only while `CANYON_PATCHING` is `on`); one of the scans is grype's, so the scans are at least 2. Past them a scan or build waits its turn and says so, a patch waits its turn too (its slot frees once the patched app runs: its watch takes none), and none fails for waiting. Raise them with the cluster's capacity, and keep the executor's `CANYON_EXECUTOR_BUILDS` at or above the builds plus the patches, and its `CANYON_EXECUTOR_SCANS` at or above the scans plus the patches plus two. |
| `CANYON_TRUSTED_PROXIES` | Optional: the addresses your load balancer connects from, comma-separated CIDRs or addresses — for an ALB with target type `ip`, its subnets. Only a request from one of them is believed about its client (`X-Forwarded-For`, which the proxy must append to — an ALB's default), for the sign-in rate limit, the device page's "Requested from" and the audit trail's `source`. Never list a range that holds nodes, pods or other hosts: any address in it can claim to be anyone. Unset, the connection's address is the client. |
| `CANYON_APP_DATABASE_URL` | Optional: the apps database's admin login, on port 5433. Name `sslmode=verify-full` and `sslrootcert=<the server's CA file>` in this URL, or `sslmode=require` at the least, as for `CANYON_DATABASE_URL`: [Configuration](/self-host/configuration/) says what each does. |
| `CANYON_APP_DATABASE_CA_FILE` | Optional, with `CANYON_APP_DATABASE_URL`: the apps database's CA bundle as mounted in step 4, `/etc/apps-db-ca/ca.pem`. Each app's `DATABASE_URL` then verifies the server against it (`sslmode=verify-full`), so `CANYON_APP_DATABASE_URL`'s host must be the name on the server's certificate — for RDS, the instance's endpoint, never an address or an alias. An app whose database was made before you set it keeps the URL it was given: forget that URL once, with no run in flight, `DELETE FROM secrets s USING resources r WHERE s.origin = 'provisioned' AND s.name = 'DATABASE_URL' AND r.project_id = s.project_id AND r.env = s.env AND r.type = 'postgres' AND r.driver = 'builtin';` on Canyon's Postgres. Each app's next deploy, and prod's next promotion, is handed a verifying URL to the same database, its data kept, under a new password: a promotion that fails after that leaves the running prod unable to open new connections until one succeeds. |

Canyon's egress gateway, `canyon-egress`, and the server both read the Secret `canyon-egress` in
`canyon`: one key, `token`, at least 32 random characters, which the gateway sends the server's receiver
of its records on every stream. Without it neither starts. Create it once, for example
`kubectl -n canyon create secret generic canyon-egress --from-literal=token="$(openssl rand -hex 32)"`.
The server takes the records on port 7781, which its NetworkPolicy admits from the gateway's pods alone.
The gateway resolves names through your cluster DNS and reaches public addresses on TCP 80 and 443 alone,
never a private one. **Settings › Outbound** (beta) sends apps' calls through it: each environment's next deploy
or promotion then runs with `HTTPS_PROXY` set to the gateway, and its pods reach nothing else outside.
While it is on, a promotion or a rollback of a version that ran without it runs through it too, and
turning it off reaches each environment at its next deploy or promotion. The front-door check that
refuses a cluster not enforcing NetworkPolicies gates every deploy here, so turning it on asks no check
of its own.

Apply the overlay and wait for the server, the executor and the router:

```sh
kubectl apply -k <your overlay>
kubectl -n canyon rollout status deploy/canyon-executor
kubectl -n canyon rollout status deploy/canyon-server
kubectl -n canyon rollout status deploy/canyon-router
```

Once the server is up, publish the certificate it minted for the policy the gateway's check pulls from it on
port 7782 (over TLS, with a token of the gateway's own ServiceAccount, which the server checks against your
cluster's key set), for the gateway to pin, and only then wait for the gateway: its pods start once the
certificate is published, and are ready once their check holds a policy.

```sh
cert=$(kubectl -n canyon exec deploy/canyon-server -- /canyon-server egress-policy-cert) &&
  kubectl -n canyon create configmap canyon-egress-policy-cert --from-literal=tls.crt="$cert" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n canyon rollout status deploy/canyon-egress
```

It holds no secret, and running the step again is safe. It lasts ten years. To renew it, delete its row on Canyon's Postgres (`DELETE FROM egress_policy_cert;`), restart the server, which mints another, run the step again, and restart the gateway (`kubectl -n canyon rollout restart deploy/canyon-egress`) so it pins the new one. A deploy routed through the gateway waits, at most 30 s, until
each replica of the check has taken the allowlist that names it, and fails if one has not.

The server creates and migrates its schema in `CANYON_DATABASE_URL` when it boots, and starts with
an empty fleet. To check the install:

```sh
kubectl wait --for=condition=Ready clusterbuilder/canyon --timeout=15m   # kpack builds and pushes the builder minutes after the rollouts
kubectl -n canyon logs deploy/canyon-server | head -5     # auth oidc, oidc discovery complete, listening on [::]:7777
kubectl -n canyon port-forward svc/canyon-server 7777:7777 & sleep 2; curl -s -o /dev/null -w '%{http_code}\n' http://localhost:7777/ready; kill %1   # 200
```

Next, point your gateway at Canyon, terminating TLS there. Send Canyon's host to the Service
`canyon-server`, port 7777, and the app wildcards to the Service `canyon-router`, port 80, both in
`canyon`. Point the DNS for Canyon's host and the wildcards at your gateway.

The certificate must cover Canyon's host and the app wildcards: `*.preview.<domain>` and
`*.prod.<domain>` for the address form `app.env.domain`, or `*.<domain>` for `app-env.domain`. The
front-door check opens TLS to two addresses under them, `canyon-probe.preview.<domain>` and
`canyon-probe.prod.<domain>` (`canyon-probe-preview.<domain>` and `canyon-probe-prod.<domain>` in
the other form), and refuses to save while the certificate doesn't cover both.

### An example: the AWS Load Balancer Controller

This is the Ingress our EKS test cluster runs, with Canyon signing people in and the form `app.env.domain`, with its
names, certificate and subnets replaced. Its ACM certificate's names are `<domain>`,
`*.preview.<domain>` and `*.prod.<domain>`, and all three DNS names are CNAMEs to the load
balancer's address:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: canyon
  namespace: canyon # an Ingress reaches Services in its own namespace alone
  annotations:
    kubernetes.io/ingress.class: alb
    alb.ingress.kubernetes.io/scheme: internet-facing
    alb.ingress.kubernetes.io/target-type: ip
    alb.ingress.kubernetes.io/listen-ports: '[{"HTTPS": 443}, {"HTTP": 80}]'
    alb.ingress.kubernetes.io/ssl-redirect: "443"
    alb.ingress.kubernetes.io/load-balancer-attributes: idle_timeout.timeout_seconds=3600 # canyon logs -f
    alb.ingress.kubernetes.io/certificate-arn: "arn:aws:acm:<region>:<account>:certificate/<id>"
    alb.ingress.kubernetes.io/subnets: "<public subnet>,<public subnet>,<public subnet>"
spec:
  rules:
    - host: <domain>
      http:
        paths:
          - {path: /, pathType: Prefix, backend: {service: {name: canyon-server, port: {number: 7777}}}}
    - host: "*.preview.<domain>"
      http:
        paths:
          - {path: /, pathType: Prefix, backend: {service: {name: canyon-router, port: {number: 80}}}}
    - host: "*.prod.<domain>"
      http:
        paths:
          - {path: /, pathType: Prefix, backend: {service: {name: canyon-router, port: {number: 80}}}}
```

With target type `ip`, set `CANYON_TRUSTED_PROXIES` to those subnets' CIDRs, when they hold no node or pod. Where the
ALB signs people in, add `auth-type: oidc`, `auth-idp-oidc` (your issuer's endpoints and a
Secret holding the client) and `auth-scope: "openid profile email"` to the same Ingress, and give
the Service `canyon-server` the annotation `alb.ingress.kubernetes.io/auth-type: none`, since
Canyon signs its own users in. The client in that Secret must accept each app's
`https://<address>/oauth2/idpresponse`: see "What your provider must accept" in
[How people reach apps](/self-host/front-door/#who-signs-people-in).

Last, set the front door. Sign in at `CANYON_PUBLIC_URL` as one of `CANYON_ADMINS`, open
**Settings › App hosting**, choose how people sign in to apps, the domain and the address form, and
save. Saving runs the network and certificate check, and Canyon shows the fix when either fails.
[How people reach apps](/self-host/front-door/) has the choices, what your OIDC client must accept
for each, and how sharing with a person depends on the sign-in issuer.

The executor takes no token. Its NetworkPolicy, which admits the server's pods and nothing else, is
the single gate in front of it. Don't expose its Service or add a policy that admits anything else
to it. An overlay may add egress for the executor, as kind's does, but never ingress.

## A private sign-in provider for apps routed through Canyon

This is only for apps whose calls go through Canyon's egress gateway (**Settings › Outbound**, beta)
when their sign-in provider isn't public. Such an app reaches no private address, so its sign-in
sidecars can't reach your provider without this. List the provider's sign-in hosts: `<host>=<address>/32`
for a provider outside the cluster (`sso.corp.example=10.0.4.20/32`), or `<host>` for one inside it
whose sign-in host resolves to its own Service (`keycloak.idp.svc.cluster.local`). A provider inside
the cluster also needs a NetworkPolicy, `canyon-apps-signin`, that admits routed pods to its own
pods. It selects every routed pod by its `canyon.floor` label, so it covers every app, now and later.

- **With the [Helm chart](/self-host/helm/)**, one block of values sets both. The chart writes the
  executor's `CANYON_KUBE_SIGNIN_DIRECT` from `hosts`, and `canyon-apps-signin` from `inCluster`:

  ```yaml
  canyon:
    egress:
      signin:
        hosts: [keycloak.idp.svc.cluster.local]
        inCluster: {namespace: idp, podLabels: {app.kubernetes.io/name: keycloak}, port: 8443}  # the port those pods serve behind the Service's 443
  ```

  The render fails when `inCluster` lacks its namespace or labels, when a label value isn't a string,
  when its port isn't a whole number from 1 to 65535, when a host is empty, or when no host is listed
  without an address. It also fails when `inCluster` names a namespace the chart writes or installs
  into, or `kube-system`. Those namespaces are Canyon's own, the gateway's (Caddy), Pocket ID's,
  kpack's and headlamp's.
- **With plain manifests**, set the executor's `CANYON_KUBE_SIGNIN_DIRECT` in your overlay,
  comma-separated, and for a provider inside the cluster apply this policy once:

  ```yaml
  apiVersion: networking.k8s.io/v1
  kind: NetworkPolicy
  metadata: {name: canyon-apps-signin, namespace: canyon-apps}
  spec:
    podSelector: {matchLabels: {canyon.floor: "true"}}
    policyTypes: [Egress]
    egress:
      - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: <the IdP's namespace>}}, podSelector: {matchLabels: {<the IdP's own pods' labels>}}}]
        ports: [{protocol: TCP, port: <the port those pods serve behind the Service's 443>}]
  ```

Either way, point the rule at the provider's own pods, **never** at an ingress's, a gateway's or a
load balancer's. Those pods serve everything else too, and every routed app would reach all of it,
unrecorded. If the provider is reachable only through your ingress, use a provider outside the
cluster, or leave egress off. Then check it: deploy one app routed through Canyon and sign in to it.

These are the rules for `CANYON_KUBE_SIGNIN_DIRECT`:

- A host is a lower-case DNS name or an IPv4 address, with no wildcard, leading dot or port, named
  once. An address is one IPv4 address with its `/32`, outside `169.254.0.0/16`. Anything else (an
  empty entry, a range, an address without `/32`, IPv6) stops the executor from starting.
- Only the front door's sign-in hosts you list are called directly: the issuer where Canyon signs people in,
  the issuer and key set behind a standard OIDC gateway, nothing behind an AWS load balancer. Any other host, a public IdP's
  included, still goes through the egress gateway, so listing your internal IdP never breaks an app
  that signs in at Google, Okta or Entra. A listed host's subdomains are called directly too, but
  reach only the listed addresses on 443.
- Every endpoint the sidecars call must be on a listed host, the discovery document's
  `token_endpoint` and `jwks_uri` included. Every front-door URL on a listed host must be on port
  443: a deploy whose front door names a listed host on another port is refused.
- With `<host>=<address>/32`, every routed app's own NetworkPolicy admits TCP 443 to that address. No
  `/32` works inside the cluster: policies match after kube-proxy has rewritten a Service's or a load
  balancer's address to a pod's. A `<host>` without an address is only for an IdP whose sign-in host
  resolves, inside the cluster, to its own Service, admitted by the policy above.

Two costs come with it:

- the whole pod, the app's own code included, can reach the listed addresses on 443, or, inside the
  cluster, the IdP's pods on the port the policy above names;
- those calls bypass the egress gateway, so Canyon records none of them.

Each deploy writes the policy and the variables. A change to the setting reaches a running app only
at its next deploy: removing an entry, or the whole setting, leaves the hole open to apps already
running until each is deployed again. The sidecar update after an upgrade changes only the sidecar's
image.

An IdP published only through your cluster's own gateway or ingress (Dex, Keycloak or Zitadel behind
it, or the Helm chart's Pocket ID on k3s) can't be reached this way. Set the server's
`CANYON_KUBE_GATEWAY_SIGNIN` to its issuer. Canyon then refuses to route apps through its egress
gateway while the front door signs in there, and warns when such a front door is saved while apps are
routed ([configuration](/self-host/configuration/)).

## Pod Security

Each of Canyon's namespaces enforces a [Pod Security](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
level, and warns and audits at the same level, so the API server refuses a pod that breaks it, whoever makes the pod:

| Namespace | Level | Why |
|---|---|---|
| `canyon`, `canyon-builds`, `canyon-scans` | `restricted` | Every pod Canyon runs there is already locked down |
| `canyon-apps` | `baseline` | An app runs as whatever user its image names, root included, which `restricted` refuses. `baseline` still refuses a privileged pod, the host's network, processes and paths, and any added capability |
| `canyon-infra`, `canyon-auth`, `canyon-gateway` (the Helm chart's bundled parts) | `restricted` | The chart runs each as no root, under the runtime's seccomp profile, every capability dropped but Caddy's `NET_BIND_SERVICE` for ports 80 and 443 |

Headlamp's namespace is made by its own chart, and Canyon doesn't label it.

Neither level stops a pod from mounting a Secret in its own namespace. Canyon's roles and the executor's own checks
cover that.

The labels name no version, so each namespace follows the newest version of its level that the cluster knows. That
is deliberate: it is the stricter choice and fails closed. A cluster upgrade can then refuse a pod that passed
before. To see which running pods a level would refuse, ask the API server without changing anything:
`kubectl label --dry-run=server --overwrite ns <namespace> pod-security.kubernetes.io/enforce=<level>`.

## Limits

- **The server runs one replica.** Its runs no longer keep it at one; its volume, one mount, does,
  and its rate limits are per process. The volume holds the key every secret is sealed under and the
  package store: losing it loses both, so back it up.
- **The egress gateway caps no one app's connections**, unlike Docker's. A connection that sends no
  request is closed after 60 s, sooner as a replica fills, and each replica holds 16,384 connections
  and as many calls out. Nothing caps one pod, and the Service spreads its connections over both
  replicas: one app pod that holds tunnels, each moving a byte within every 5 minutes and opened
  again at its 15-minute cap, can fill both and deny egress to every app on the cluster.
- **Apps' DNS lookups through the cluster's resolver stay open** while their calls go through
  Canyon, until a later release closes them.

## Strict mode on the AWS VPC CNI

On EKS, Canyon needs two settings in the `vpc-cni` add-on's configuration values: `enableNetworkPolicy`
`"true"`, which runs the network policy agent beside each node's CNI, and `NETWORK_POLICY_ENFORCING_MODE`
`"strict"`, which holds a new pod's traffic until its policy is in force. In the default `standard` mode
a new pod gets out for its first moments, and the front door's check answers so. Without both, Canyon
builds nothing, under hand-off as well, and every deploy ends with "Canyon hasn't seen this cluster
enforce NetworkPolicy, so it builds nothing here".

Check what the add-on has now:

```sh
aws eks describe-addon --cluster-name <cluster> --addon-name vpc-cni \
  --query addon.configurationValues --output text
```

The answer must hold both, alongside any other values you set:

```json
{"enableNetworkPolicy": "true", "env": {"NETWORK_POLICY_ENFORCING_MODE": "strict"}}
```

To set them, give `update-addon` the whole configuration: it replaces what was there, so merge in your
other values first. Apply the two policies below before you do.

```sh
aws eks update-addon --cluster-name <cluster> --addon-name vpc-cni \
  --configuration-values '{"enableNetworkPolicy": "true", "env": {"NETWORK_POLICY_ENFORCING_MODE": "strict"}}'
```

Once the update is `ACTIVE`, `kubectl -n kube-system get ds aws-node -o jsonpath='{.spec.template.spec.containers[*].name}'`
lists `aws-eks-nodeagent`. Then save the front door again in **Settings › App hosting**: the save runs
the check, and the next deploy builds.

Canyon doesn't watch the cluster after a save. If enforcement is turned off, the add-on goes back to
standard mode, or new nodes come up without the policy agent, builds go on until the front door is
saved again, and the check runs on one node. Give the nodes IMDSv2 with a hop limit of 1 as the
backstop: a pod then doesn't reach the node's role even where a policy is not in force.

With `NETWORK_POLICY_ENFORCING_MODE` `strict`, a pod that no NetworkPolicy selects gets no traffic at
all, and that includes the cluster's own add-ons. Canyon's four namespaces ship their policies;
`kube-system` and `kpack` need theirs before anything there can reach DNS, AWS or the API server.
Without them CoreDNS and the EBS CSI driver crash-loop, and their add-ons never leave `CREATING`.
Our EKS test cluster applies these two, which let those namespaces' pods do what they did before strict
mode. Tighter rules are yours to write:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: allow-all, namespace: kube-system}
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
  ingress: [{}]
  egress: [{}]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: allow-all, namespace: kpack} # after kpack's first apply creates the namespace
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
  ingress: [{}]
  egress: [{}]
```

## Host firewall

Some network plugins don't apply a pod's NetworkPolicy to its traffic to its own node. On EKS, the
AWS VPC CNI doesn't, even in strict mode: an app reaches the kubelet (10250), sshd (22) and the
CNI's own introspection port on the server it runs on, though no other node. Calico does apply it.
Canyon's front-door check tries 10250 and 22 from a probe pod and, when either answers, saves the
front door and warns: `canyon status` names the ports, and the dashboard links here. Closing them is
a rule on each node, yours to add.

On EKS (AL2023, `iptables-nft`), this drops every new connection a pod opens to its own node, and
keeps the replies to the kubelet's health checks:

```sh
iptables -N CANYON-POD-TO-NODE
iptables -A CANYON-POD-TO-NODE -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
iptables -A CANYON-POD-TO-NODE -j DROP
iptables -I INPUT 1 -i eni+ -j CANYON-POD-TO-NODE
```

`eni+` names the VPC CNI's pod interfaces. Apply it at every node's boot, from your launch
template's user data, or from a privileged DaemonSet with `hostNetwork` of your own. With it in place
on an EKS cluster with the VPC CNI, the warning went away, DNS still resolved and the apps kept serving. Then save
the front door again: the warning is the latest check's.

