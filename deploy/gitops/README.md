# The deploy repo's reference set-up

What a platform team applies once so Canyon can hand releases to its cluster through a deploy repo
(`delivery: repo`, design/HAND-OFF.md §3b). Canyon's dashboard shows Argo CD's filled in for the host
(Settings › App hosting › *For your platform team*); Canyon's kind leg (.github/workflows/kube.yml, `handoff-repo`)
applies them as they are here, once under Argo CD and once under Flux; the docs explain each (self-host: *Your deploy repo*, *Argo CD*, *Flux*,
*External Secrets*).

| File | Apply it | What it does |
|---|---|---|
| `argocd/appprojects.yaml` | in Argo CD's namespace | one AppProject per environment: the deploy repo alone, `canyon-*-<env>` namespaces alone, no cluster-scoped object, exactly the kinds Canyon and the templates make |
| `admission/services.yaml` | once per cluster, Kubernetes 1.30 or later | a ValidatingAdmissionPolicy and its binding: in `canyon-*-<env>` namespaces a Service is `ClusterIP` alone, with no `externalIPs` or `loadBalancerClass`, never a load balancer or a node port. An AppProject restricts kinds, not fields; under Flux it is the same file |
| `argocd/applicationsets.yaml` | in Argo CD's namespace | one ApplicationSet per branch: an Application per app, its project and namespace from the path, its first sync retried until the namespace is there |
| `argocd/namespaces.yaml` | in Argo CD's namespace | each app environment's namespace, from your platform repo's copy of `namespaces/`: an Application per app and environment, and an AppProject that may make Namespaces and nothing else |
| `namespaces/` | **into your platform repo**, never the deploy repo | the chart those Applications, or Flux's HelmReleases, render: the Namespace `canyon-<app>-<env>`, Pod Security `baseline`, labelled `canyon.dev/env: <env>`. Argo CD's mode takes `app` and `env` and makes that one namespace; Flux's takes `root` and `env` and makes, for each app's `canyon.json` its ArtifactGenerator lays beside it, the namespace, a RoleBinding for the environment's sync account and the app's Kustomization, reading the paths alone |
| `gateway/gateway.yaml` | an example for your own Gateway, never applied as it is | one listener per environment, `*.preview.<domain>` and `*.prod.<domain>`, each admitting routes only from namespaces labelled with its environment, so a route pushed to preview can't claim a production hostname |
| `argocd/repository.yaml` | in Argo CD's namespace | Argo CD's read-only credential for the deploy repo, never Canyon's token |
| `external-secrets/clustersecretstore.yaml` | once per cluster | the store each release's access token comes through, under the fetch-only token, for `canyon-` namespaces alone |
| `flux/namespaces.yaml` | once per cluster | Flux's two namespaces beside `flux-system`: `canyon-sync`, the deploy repo's sync, and `canyon-namespaces`, the generator's Helm records; a quota on app Kustomizations |
| `flux/rbac.yaml` | once per cluster | the sync accounts `canyon-preview` and `canyon-prod` and their ClusterRole `canyon-deploy`, exactly the AppProjects' kinds; the namespaces generator's account `canyon-namespaces` and its grants, nothing in `flux-system` |
| `flux/admission.yaml` | once per cluster, Kubernetes 1.30 or later, with `rbac.yaml` | a ValidatingAdmissionPolicy on the generator's account: Canyon's own namespaces, RoleBindings and Kustomizations alone, every reference and account pinned, no other field admitted. Not optional: the chart renders nothing without it |
| `flux/repository.yaml` | in `canyon-sync` and `flux-system` | Flux's read-only credentials for the deploy repo and the platform repo, never Canyon's token |
| `flux/sources.yaml` | in `canyon-sync` and `flux-system` | the deploy repo's branches for the sync (kustomization files left out) and for the generator (`canyon.json` and the route template alone), the platform repo, and one ArtifactGenerator per environment laying the chart beside the app names |
| `flux/generator.yaml` | in `flux-system` | one HelmRelease per environment rendering the chart from its artifact as `canyon-namespaces` |
| `templates/.canyon/` | **into the deploy repo**, under its root | the templates Canyon renders each app's route and resources from |

Every file but the templates goes outside the deploy repo: whoever can write the deploy repo must not be able
to change what may be deployed from it. Namespaces come from your platform repo for the same reason: a token that can
write the deploy repo can never choose a namespace's pod security. The placeholders are `${CANYON_URL}`, `${REPO_URL}`, `${ROOT}` and
`${APP_SEGMENT}` (the number of segments in the root, `1` for `apps`), `${PREVIEW_BRANCH}`, `${PROD_BRANCH}`,
`${STORE}`, `${REQUEUE_SECONDS}` (how often Argo CD looks for new apps; `180` is its default), `${READ_USER}` and
`${READ_TOKEN}`, `${PLATFORM_REPO_URL}`, `${PLATFORM_REVISION}` and `${PLATFORM_PATH}` (where your platform repo
holds `namespaces/`), under Flux `${PLATFORM_READ_USER}` and `${PLATFORM_READ_TOKEN}` (its read credential) and
`${INTERVAL}` (how often Flux reads and reconciles; `1m`), and `${CA_BUNDLE}`: empty, or `      caBundle: <base64 of the CA that signed Canyon's address>`.

Everything in this directory is under the Zero-Clause BSD licence (SPDX `0BSD`, LICENSE here), not Canyon's
AGPL-3.0: copy it, change it and share it freely. It ships in `canyon-deploy.tar.gz` with every release.
