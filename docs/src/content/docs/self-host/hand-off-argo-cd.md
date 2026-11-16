---
title: Argo CD
description: Sync Canyon's deploy repo to your cluster with Argo CD. The AppProjects and ApplicationSets to apply once, Argo CD's read-only credential, a private CA, how fast a release syncs, and reporting a failed sync.
---

This page is for platform teams whose Argo CD syncs the deploy repo Canyon commits releases to
([Your deploy repo](/self-host/hand-off/#your-deploy-repo)). After reading it, you can apply
the reference set-up, so that each app Canyon commits becomes an Argo CD Application in its own
namespace, and know what each part guards against. Canyon's own tests run this set-up on Argo CD.

You apply these once, all of them **outside the deploy repo**: whoever can write the deploy repo must
not be able to change what may be deployed from it. Three AppProjects (`canyon-preview`, `canyon-prod`
and `canyon-namespaces`), the two apps' ApplicationSets, the namespaces' ApplicationSet and the
repository credential go in Argo CD's namespace; the admission policy on the apps' Services is the
cluster's; the ClusterSecretStore and the Secret `canyon-fetch-token` are External Secrets'. One more goes into your **platform repo**, the one
Canyon's token can't write: the chart that makes each app's namespace. Don't skip it: without it, no
app's namespace is made and no app goes live. [What your platform team sets up
once](/self-host/hand-off/#what-your-platform-team-sets-up-once) puts every step in order.

The cluster must run **Kubernetes 1.30 or later**: the admission policy on the apps' Services is a
ValidatingAdmissionPolicy, an API generally available from 1.30. Before the first deploy, Canyon's own
cluster, where it builds every image, must also enforce NetworkPolicy: Canyon builds nothing where its
check hasn't seen it enforced ([What it needs](/self-host/hand-off/#what-it-needs)).

| File | What it does |
|---|---|
| `argocd/appprojects.yaml` | One AppProject per environment: the deploy repo alone, `canyon-*-<env>` namespaces alone, no cluster-scoped object, exactly the kinds Canyon and your templates make |
| `admission/services.yaml` | Once per cluster: in the apps' namespaces a Service is `ClusterIP` alone, never a load balancer, a node port or an external IP. See [The apps' Services](#the-apps-services) |
| `gateway/gateway.yaml` | An example of your own Gateway, not applied as it is: one listener per environment, each admitting only its own environment's routes. See [The gateway](#the-gateway) |
| `argocd/applicationsets.yaml` | One ApplicationSet per branch: an Application per app, its project and namespace from the path |
| `argocd/namespaces.yaml` | Each app environment's namespace, from your platform repo: an ApplicationSet, and an AppProject that may make Namespaces and nothing else |
| `namespaces/` | The chart those Applications render, **in your platform repo**: exactly the Namespace `canyon-<app>-<env>`, Pod Security `baseline` |
| `argocd/repository.yaml` | Argo CD's read-only credential for the deploy repo, never Canyon's token |
| `external-secrets/clustersecretstore.yaml` | The store each release's access token comes through: see [External Secrets](/self-host/hand-off-external-secrets/) |

The files are in `deploy/gitops` in Canyon's source. **Settings › App hosting › For your platform
team** shows them filled in for your Canyon, ready to copy. The reference files in `deploy/gitops` are under the Zero-Clause BSD licence (0BSD,
`deploy/gitops/LICENSE`), not Canyon's: copy them, change them and share them freely. They also come in
`canyon-deploy.tar.gz` with every release. Here they are with their
placeholders:

| Placeholder | What to put |
|---|---|
| `${REPO_URL}` | The deploy repo's address, as in Canyon's **Repository URL** |
| `${PREVIEW_BRANCH}`, `${PROD_BRANCH}` | The two branches, as in Canyon's form |
| `${ROOT}` | **Path in the repo**: `apps` unless you chose another |
| `${APP_SEGMENT}` | The number of segments in `${ROOT}`: `1` for `apps`, `2` for `deploy/apps` |
| `${REQUEUE_SECONDS}` | How often Argo CD looks for a new app's `canyon.json`: `180` is its default |
| `${READ_USER}`, `${READ_TOKEN}` | Argo CD's own read-only credential for the repo. Put the token in with `kubectl`, never in a file ([Argo CD's credential for the repo](#argo-cds-credential-for-the-repo)) |
| `${PLATFORM_REPO_URL}`, `${PLATFORM_REVISION}`, `${PLATFORM_PATH}` | Your platform repo, its branch, and where in it you put `namespaces/` |

## The AppProjects

```yaml
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: canyon-preview
  namespace: argocd
spec:
  description: Canyon's previews, from the deploy repo
  sourceRepos:
    - ${REPO_URL}
  destinations:
    - server: https://kubernetes.default.svc
      namespace: canyon-*-preview
  clusterResourceWhitelist: []
  namespaceResourceWhitelist:
    - {group: apps, kind: Deployment}
    - {group: "", kind: Service}
    - {group: "", kind: ConfigMap}
    - {group: networking.k8s.io, kind: NetworkPolicy}
    - {group: external-secrets.io, kind: ExternalSecret}
    - {group: generators.external-secrets.io, kind: Password}
    - {group: gateway.networking.k8s.io, kind: HTTPRoute}
    - {group: postgresql.cnpg.io, kind: Cluster}
---
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: canyon-prod
  namespace: argocd
spec:
  description: Canyon's production, from the deploy repo
  sourceRepos:
    - ${REPO_URL}
  destinations:
    - server: https://kubernetes.default.svc
      namespace: canyon-*-prod
  clusterResourceWhitelist: []
  namespaceResourceWhitelist:
    - {group: apps, kind: Deployment}
    - {group: "", kind: Service}
    - {group: "", kind: ConfigMap}
    - {group: networking.k8s.io, kind: NetworkPolicy}
    - {group: external-secrets.io, kind: ExternalSecret}
    - {group: generators.external-secrets.io, kind: Password}
    - {group: gateway.networking.k8s.io, kind: HTTPRoute}
    - {group: postgresql.cnpg.io, kind: Cluster}
```

Each project reads the deploy repo alone, deploys into its own environment's `canyon-` namespaces
alone, and makes no cluster-scoped object. Its list of kinds is exactly what Canyon commits
(Deployment, Service, NetworkPolicy, ExternalSecret, and the ConfigMap for Canyon's CA) and what the
reference templates make (an HTTPRoute, and for a database a CloudNativePG Cluster with a generated
Password). When your own templates make other kinds, such as an Ingress or a bucket claim, add those
kinds, and nothing else. A sync of a kind the project doesn't list fails, and the Application says
which kind.

## The apps' Services

An AppProject decides which kinds the deploy repo may make, but not what is in them. A Service is
one of those kinds, so without more, whoever can write the deploy repo could commit a Service that
reaches past the app's namespace:

- `type: LoadBalancer` makes a load balancer in your cloud account. On EKS, the AWS Load Balancer
  Controller makes a new NLB, internet-facing if the Service's annotations say so, and opens your
  nodes' security groups to it.
- `type: NodePort` opens a port on every node.
- `spec.externalIPs` can intercept traffic meant for other addresses, other teams' included
  (CVE-2020-8554). EKS doesn't turn on the `DenyServiceExternalIPs` admission plugin.

`admission/services.yaml` closes that: a ValidatingAdmissionPolicy and its binding that admit, in
the apps' namespaces, a Service of type `ClusterIP` alone, with no `externalIPs` and no
`loadBalancerClass`. Canyon's own Service for each app is one, and so are the ones CloudNativePG
makes for a database. People reach an app through your gateway's route, never through a Service of
its own. Apply it once per cluster, outside the deploy repo, with `kubectl apply -f
admission/services.yaml`. It needs **Kubernetes 1.30 or later**, where ValidatingAdmissionPolicy is
generally available. It has no placeholders.

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: canyon-app-services
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: [""]
        apiVersions: ["*"]
        operations: ["CREATE", "UPDATE"]
        resources: ["services"]
  matchConditions:
    - name: canyon-app-namespace
      expression: request.namespace.matches('^canyon-.*-(preview|prod)$')
  validations:
    - expression: object.spec.type == 'ClusterIP' && !has(object.spec.externalIPs) && !has(object.spec.loadBalancerClass)
      reason: Forbidden
      message: "Canyon's app namespaces take ClusterIP Services alone, with no externalIPs or loadBalancerClass: people reach an app through your gateway's route, never a load balancer, a node port or an external IP."
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: canyon-app-services
spec:
  policyName: canyon-app-services
  validationActions: [Deny]
```

It holds for every Service in a namespace whose name matches `^canyon-.*-(preview|prod)$`, which is
every namespace the AppProjects' `canyon-*-preview` and `canyon-*-prod` admit, whoever makes the
Service. The match is on the namespace's name, which never changes, and a Service is always in the
namespace its request names, so no label or field on the Service takes it out of scope. With
`failurePolicy: Fail`, a policy the cluster can't evaluate refuses the Service rather than letting it
through. A sync it refuses fails, and the Application shows the policy's message.

## The gateway

Both environments' routes attach to your Gateway, and the preview branch takes pushes nobody
reviews. If one listener served both, an HTTPRoute committed to preview could claim a production
hostname, such as `claims-portal.prod.apps.example.com`, with a more specific match such as
`/login`. Gateway API sends each request to the most specific match, so that path would be served by
code nobody reviewed, under the production hostname, and would receive its cookies. Production's
review would be bypassed.

Give your Gateway **one listener per environment**, each taking only its own environment's
hostnames and admitting routes only from namespaces labelled with that environment. The namespaces
chart labels every app namespace `canyon.dev/env: preview` or `canyon.dev/env: prod`, from the
branch its generator reads ([Namespaces](#namespaces)). Nothing synced from the deploy repo can set
that label, because no AppProject of the deploy repo may make a Namespace. `gateway/gateway.yaml`
is an example to adapt: set the class, the domain and the certificates to your own.

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: apps
  namespace: gateway
spec:
  gatewayClassName: example
  listeners:
    - name: preview
      protocol: HTTPS
      port: 443
      hostname: "*.preview.apps.example.com"
      tls:
        mode: Terminate
        certificateRefs:
          - name: apps-preview-tls
      allowedRoutes:
        kinds:
          - kind: HTTPRoute
        namespaces:
          from: Selector
          selector:
            matchLabels:
              canyon.dev/env: preview
    - name: prod
      protocol: HTTPS
      port: 443
      hostname: "*.prod.apps.example.com"
      tls:
        mode: Terminate
        certificateRefs:
          - name: apps-prod-tls
      allowedRoutes:
        kinds:
          - kind: HTTPRoute
        namespaces:
          from: Selector
          selector:
            matchLabels:
              canyon.dev/env: prod
```

A preview route that claims a production hostname is then accepted by neither listener. The
production listener admits no preview namespace, and the preview listener's hostname doesn't cover
it. The route reports `Accepted: False`, and the production hostname keeps serving production. The
reference `route.yaml` names its environment's listener, `sectionName: ${env}`
([Templates](/self-host/hand-off/#templates)). If your apps' hostnames don't put the environment in
the name, give each environment a domain of its own and keep one listener for each.

Routes aren't pinned to a backend. Whoever can write the deploy repo also writes the app's
Deployment, so pinning a route's backend to the sidecar's port would hold back nothing they can't
change beside it.

## The ApplicationSets

```yaml
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: canyon-preview
  namespace: argocd
spec:
  goTemplate: true
  goTemplateOptions: ["missingkey=error"]
  generators:
    - git:
        repoURL: ${REPO_URL}
        revision: ${PREVIEW_BRANCH}
        files:
          - path: ${ROOT}/*/preview/canyon.json
        requeueAfterSeconds: ${REQUEUE_SECONDS}
  syncPolicy:
    preserveResourcesOnDeletion: false
  template:
    metadata:
      name: 'canyon-{{ index .path.segments ${APP_SEGMENT} }}-preview'
      labels:
        canyon.dev/env: preview
      finalizers:
        - resources-finalizer.argocd.argoproj.io
    spec:
      project: canyon-preview
      source:
        repoURL: ${REPO_URL}
        targetRevision: ${PREVIEW_BRANCH}
        path: '{{ .path.path }}'
        directory:
          recurse: true
          exclude: canyon.json
      destination:
        server: https://kubernetes.default.svc
        namespace: 'canyon-{{ index .path.segments ${APP_SEGMENT} }}-preview'
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
          allowEmpty: true # an undeploy of an app with no resources/ leaves canyon.json alone: nothing to apply, everything to prune
        retry:
          limit: 10
          backoff:
            duration: 10s
            factor: 2
            maxDuration: 3m
---
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: canyon-prod
  namespace: argocd
spec:
  goTemplate: true
  goTemplateOptions: ["missingkey=error"]
  generators:
    - git:
        repoURL: ${REPO_URL}
        revision: ${PROD_BRANCH}
        files:
          - path: ${ROOT}/*/prod/canyon.json
        requeueAfterSeconds: ${REQUEUE_SECONDS}
  syncPolicy:
    preserveResourcesOnDeletion: false
  template:
    metadata:
      name: 'canyon-{{ index .path.segments ${APP_SEGMENT} }}-prod'
      labels:
        canyon.dev/env: prod
      finalizers:
        - resources-finalizer.argocd.argoproj.io
    spec:
      project: canyon-prod
      source:
        repoURL: ${REPO_URL}
        targetRevision: ${PROD_BRANCH}
        path: '{{ .path.path }}'
        directory:
          recurse: true
          exclude: canyon.json
      destination:
        server: https://kubernetes.default.svc
        namespace: 'canyon-{{ index .path.segments ${APP_SEGMENT} }}-prod'
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
          allowEmpty: true # an undeploy of an app with no resources/ leaves canyon.json alone: nothing to apply, everything to prune
        retry:
          limit: 10
          backoff:
            duration: 10s
            factor: 2
            maxDuration: 3m
```

What each part does:

- **One ApplicationSet per branch.** The preview set reads the preview branch alone and the
  production set the production branch alone, so a file on the preview branch can never reach
  production.
- **The files generator** finds `<root>/*/<env>/canyon.json` on its branch: one Application for each app
  Canyon has committed to that environment, named `canyon-<app>-<env>`.
- **The project and the namespace come from the path**, never from `canyon.json`. The project is the
  ApplicationSet's own, and the namespace is `canyon-` and the app's path segment and the environment.
  `canyon.json` only tells the generator an app is there.
- **The source is the app's directory**, read recursively so `resources/` is included, with
  `canyon.json` excluded: it isn't a Kubernetes object.
- **Automated sync with prune and self-heal**: a commit is applied without anyone clicking, a file
  Canyon removes is deleted from the cluster, and a hand edit in the cluster is undone.
  **`allowEmpty: true`**: an undeploy of an app that declares no database or bucket leaves only
  `canyon.json`, so nothing is left to apply. Argo CD's default refuses to prune an Application down to
  nothing, and the app would keep running; with it, the app is pruned like any other. It also means an
  edit of yours to the source's `directory` include or exclude that matches nothing prunes every app,
  as it would under Flux: check such an edit against a preview first.
- **No namespace is made here.** The namespace comes from your platform repo
  ([Namespaces](#namespaces)). Until it exists, an app's first sync fails, and `retry` tries it again:
  10 seconds, then twice as long each time, up to 3 minutes apart.
- **The `resources-finalizer`** and `preserveResourcesOnDeletion: false`: when Canyon deletes an app's
  directory, the ApplicationSet deletes its Application, and Argo CD deletes everything the Application
  made first, the database and its storage claim included. The namespace's own Application goes with
  the same directory, and deletes the namespace and everything left in it. The data is gone.

When Canyon undeploys an environment, or production is held, it keeps `canyon.json` and `resources/`
([Removes and deletes](/self-host/hand-off/#removes-and-deletes)), so the Application and the
namespace's Application both stay, Argo CD prunes the app, and the namespace, the database and its
data stay.

The same settings decide what happens when you delete an ApplicationSet yourself: it deletes every
Application it made, and everything they made. To take the set-up down and keep the apps, see
[Taking the set-up down](/self-host/hand-off/#taking-the-set-up-down).

## Namespaces

Each app environment's namespace, `canyon-<app>-<env>`, comes from **your platform repo**, never from
the deploy repo. Whoever holds a token that can write the deploy repo can commit any file there. If
the namespace came from that repo, they could commit one with `pod-security.kubernetes.io/enforce:
privileged`, and the `baseline` your pods run under would be gone. So the deploy repo only names
the namespace, through its directory, and your platform repo says what the namespace is.

Copy `deploy/gitops/namespaces` into your platform repo. It's a Helm chart that renders exactly
one Namespace from two values: `app`, which must be a DNS label, and `env`, which must be `preview`
or `prod`. It refuses anything else, and a name longer than 63 characters:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: canyon-<app>-<env>
  labels:
    pod-security.kubernetes.io/enforce: baseline
    canyon.dev/managed: "true"
    canyon.dev/env: <env>
```

`canyon.dev/env` is what [your gateway's listeners](#the-gateway) admit routes by.

Then apply `argocd/namespaces.yaml`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: canyon-namespaces
  namespace: argocd
spec:
  description: The namespaces of Canyon's apps, from the platform repo
  sourceRepos:
    - ${PLATFORM_REPO_URL}
  destinations: # Canyon's app namespaces alone, never its own canyon-apps or canyon-egress
    - server: https://kubernetes.default.svc
      namespace: canyon-*-preview
    - server: https://kubernetes.default.svc
      namespace: canyon-*-prod
  clusterResourceWhitelist:
    - {group: "", kind: Namespace}
  namespaceResourceBlacklist:
    - {group: "*", kind: "*"}
---
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: canyon-namespaces
  namespace: argocd
spec:
  goTemplate: true
  goTemplateOptions: ["missingkey=error"]
  generators:
    - git: # the preview branch alone, its own environment's files alone: a preview commit never makes a prod namespace
        repoURL: ${REPO_URL}
        revision: ${PREVIEW_BRANCH}
        files:
          - path: ${ROOT}/*/preview/canyon.json
        requeueAfterSeconds: ${REQUEUE_SECONDS}
        values:
          env: preview
    - git:
        repoURL: ${REPO_URL}
        revision: ${PROD_BRANCH}
        files:
          - path: ${ROOT}/*/prod/canyon.json
        requeueAfterSeconds: ${REQUEUE_SECONDS}
        values:
          env: prod
  syncPolicy:
    preserveResourcesOnDeletion: false
  template:
    metadata:
      name: 'canyon-{{ index .path.segments ${APP_SEGMENT} }}-{{ .values.env }}-ns'
      finalizers:
        - resources-finalizer.argocd.argoproj.io
    spec:
      project: canyon-namespaces
      source:
        repoURL: ${PLATFORM_REPO_URL}
        targetRevision: ${PLATFORM_REVISION}
        path: ${PLATFORM_PATH}
        helm:
          # Structured values, never parameters: helm's --set would read a comma or a dot in a directory's name as
          # another value. The chart checks app as a DNS label and env as preview or prod, and makes the name.
          valuesObject:
            app: '{{ index .path.segments ${APP_SEGMENT} }}'
            env: '{{ .values.env }}'
      destination:
        server: https://kubernetes.default.svc
        namespace: 'canyon-{{ index .path.segments ${APP_SEGMENT} }}-{{ .values.env }}'
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
```

- **The same generators as the apps'**: one namespace for each `canyon.json`, so the namespace exists
  exactly while the app's Application does. Each namespace's Application is named
  `canyon-<app>-<env>-ns`, which ends in `-ns` where an app's own Application ends in its environment,
  so the two never collide, whatever an app is called.
- **Only the app's name comes from the deploy repo**, from the directory's path, never from
  `canyon.json`. The environment is each generator's own, so a commit to the preview branch can never
  make a production namespace. Both reach the chart as structured values, never `--set` parameters,
  which would read a comma in a directory's name as another value, and the chart checks each.
- **goTemplate substitutes** the path into each field as data: a directory named like a template is
  refused by the chart, never run.
- **Its own AppProject**: it reads your platform repo alone and may make Namespaces and nothing else,
  named `canyon-*-preview` or `canyon-*-prod` alone, so it can never touch Canyon's own namespaces,
  such as `canyon-apps` or `canyon-egress`. It has no `sourceNamespaces` and no roles, so only
  Applications in Argo CD's own namespace use it. The app projects keep `clusterResourceWhitelist: []`,
  so nothing synced from the deploy repo is ever cluster-scoped.
- **An undeploy keeps the namespace**: `canyon.json` and `resources/` stay, so the namespace, the
  database and its data stay. **Deleting the app deletes the data**: Canyon removes the directory, and
  the namespace's Application deletes the namespace and everything in it, the CloudNativePG Cluster and
  its storage claim included.

## Argo CD's credential for the repo

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: canyon-deploy-repo
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
stringData:
  type: git
  url: ${REPO_URL}
  username: ${READ_USER}
  password: ${READ_TOKEN}
```

Give Argo CD a credential that can only read: on GitHub, a fine-grained token with **Contents:
Read-only** on the deploy repo alone, or a read-only deploy key, which takes this Secret's SSH form (an
`ssh://` url and `sshPrivateKey`); on Gitea or Forgejo, an access token that reads repositories; on
GitLab, a project access token with the Reporter role and `read_repository`, or a deploy token with
`read_repository` alone. Never give it Canyon's token, which can write.

Never write the token into this file, nor into any file you commit, and never type it on a command
line, where your shell's history and the process list keep it. Create the Secret with `kubectl`,
which reads the token from its input: paste it and end with Ctrl-D, or pipe it from your secret
store. The same line then labels it so Argo CD reads it:

```sh
kubectl -n argocd create secret generic canyon-deploy-repo --from-literal=type=git --from-literal=url='${REPO_URL}' --from-literal=username='${READ_USER}' --from-file=password=/dev/stdin && kubectl -n argocd label secret canyon-deploy-repo argocd.argoproj.io/secret-type=repository
```

Keep it one line: pasted as two, without your terminal's bracketed paste, the second would be read as
the token. The platform team's card in **Settings › App hosting** gives this line with your repo's URL
and the user filled in, each single-quoted, and has no field for the token.

## A git host with a private CA

If a private CA signed your git host's certificate, Argo CD needs it too. Add it to
`argocd-tls-certs-cm`, keyed by the git host's name:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-tls-certs-cm
  namespace: argocd
  labels:
    app.kubernetes.io/name: argocd-cm
    app.kubernetes.io/part-of: argocd
data:
  git.example.com: |
    -----BEGIN CERTIFICATE-----
    <your CA's certificate>
    -----END CERTIFICATE-----
```

Canyon's server needs to trust it as well:
[A git host at a private address](/self-host/hand-off/#a-git-host-at-a-private-address).

## How fast a release syncs

A release goes live at the first check-in of its pod, so how soon Argo CD sees Canyon's commit decides
how long a deploy waits. Two clocks matter:

- **A new app**: the ApplicationSet looks for new `canyon.json` files every `${REQUEUE_SECONDS}`. Lower
  it, such as to `30`, for a first deploy that starts sooner.
- **A new release of an app**: each Application looks at its branch every `timeout.reconciliation`, set
  in `argocd-cm`, about three minutes by default.

Or skip both: point a push webhook from your git host at Argo CD's `/api/webhook`, and Argo CD looks at
once, for new apps and new releases alike.

If no pod has checked in ten minutes after Canyon's commit, the deploy says "Canyon committed v1.4 10
minutes ago and no pod has checked in: look at your Argo CD or Flux sync". Start with
`argocd app get canyon-<app>-<env>-ns`, for the namespace, then `argocd app get canyon-<app>-<env>`:
see [Troubleshooting](/self-host/hand-off/#troubleshooting).

## Reporting a failed sync

Optional. Without it, a release whose sync fails waits its 30 minutes and ends "No pod has checked in
on v1.4 in 30 minutes." With it, Argo CD tells Canyon at once, and the deploy ends with "Your platform
reported: " and the reason.

Argo CD's notifications post `{"status": "failed", "reason": "…"}` to the release's report address,
`<your Canyon>/releases/<release>/report`, with the **host token**: the deploy repo's own, shown once
beside the fetch-only token at the repo's first save, or the webhook's, if one is saved; either works.
Lost, it is replaced with **Replace the host token** in the repo's step. Reporting failures is the
only thing that brings the host token into your cluster: it can report on any release and fetch access tokens, so keep it in
`argocd-notifications-secret` alone.

1. Put the host token in `argocd-notifications-secret`, key `canyon-host-token`, from your secret store.
2. Add the service, the template and the trigger to `argocd-notifications-cm`:

   ```yaml
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: argocd-notifications-cm
     namespace: argocd
   data:
     service.webhook.canyon: |
       url: ${CANYON_URL}
       headers:
         - name: Authorization
           value: Bearer $canyon-host-token
         - name: Content-Type
           value: application/json
     template.canyon-failed: |
       webhook:
         canyon:
           method: POST
           path: /releases/{{ index .app.metadata.annotations "canyon.dev/release" }}/report
           body: |
             {"status": "failed", "reason": {{ printf "Argo CD: %s" .app.status.operationState.message | trunc 1000 | toJson }}}
     trigger.on-canyon-sync-failed: |
       - when: app.status.operationState != nil and app.status.operationState.phase in ['Error', 'Failed']
         oncePer: app.status.sync.revision
         send: [canyon-failed]
   ```

   `${CANYON_URL}` is your Canyon's address, `CANYON_PUBLIC_URL`.
3. In each ApplicationSet's `template.metadata`, add the release from `canyon.json` and the
   subscription:

   ```yaml
   template:
     metadata:
       annotations:
         canyon.dev/release: '{{ .release }}'
         notifications.argoproj.io/subscribe.on-canyon-sync-failed.canyon: ""
   ```

The release is read from `canyon.json` only to say which release failed: a `failed` report can end a
deploy, never make one live. A `failed` report ends the deploy, so report only a failure that is
final, such as a sync Argo CD gave up on, not a pod that is still starting.

## Related

- [Your platform runs the apps](/self-host/hand-off/): hand-off, the deploy repo and what Canyon commits.
- [External Secrets](/self-host/hand-off-external-secrets/): the ClusterSecretStore and the fetch-only token.
- [Flux](/self-host/hand-off-flux/): the same set-up for Flux.
