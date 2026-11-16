---
title: Flux
description: Sync Canyon's deploy repo to your cluster with Flux. The controllers and flags it needs, the namespaces made from your platform repo with no hand step, the accounts Flux acts as, the admission policy that bounds them, and what Flux can't tell Canyon.
---

:::note[The versions Canyon's CI runs]
Canyon's CI runs this set-up end to end on every change to it, beside
[Argo CD](/self-host/hand-off-argo-cd/)'s, on Flux 2.9.6: source-controller and kustomize-controller
1.9.6, helm-controller 1.6.5 and source-watcher 2.2.4, with ArtifactGenerator
`source.extensions.fluxcd.io/v1beta1` and helm-controller's `ExternalArtifact` feature gate.
:::

This page is for platform teams whose Flux syncs the deploy repo Canyon commits releases to
([Your deploy repo](/self-host/hand-off/#your-deploy-repo)). After reading it, you can apply the
reference set-up for Flux, so that each app Canyon commits gets its own namespace, made from your
platform repo with no hand step, and its own Kustomization applying its directory into it. You'll also
know what each part guards against.

You apply these once, all of them **outside the deploy repo**: whoever can write the deploy repo must
not be able to change what may be deployed from it. You also apply the
[External Secrets](/self-host/hand-off-external-secrets/) ClusterSecretStore and
[the admission policy on the apps' Services](/self-host/hand-off-argo-cd/#the-apps-services), as with
Argo CD, and copy the namespaces chart into **your platform repo**, the one Canyon's token can't write.
Both admission policies need **Kubernetes 1.30 or later**, where ValidatingAdmissionPolicy is generally
available. Before the first deploy, Canyon's own cluster, where it builds every image, must also enforce
NetworkPolicy: Canyon builds nothing where its check hasn't seen it enforced
([What it needs](/self-host/hand-off/#what-it-needs)).

| File | What it does |
|---|---|
| `flux/namespaces.yaml` | The two namespaces Flux's set-up works in, `canyon-sync` and `canyon-namespaces`, and a quota on app Kustomizations |
| `flux/rbac.yaml` | The accounts Flux acts as: the two sync accounts and their ClusterRole `canyon-deploy`, and the namespaces generator's account |
| `flux/admission.yaml` | The admission policy that holds the generator's account to Canyon's own names. Not optional |
| `flux/repository.yaml` | Flux's read-only credentials for the deploy repo and your platform repo, never Canyon's token |
| `flux/sources.yaml` | The GitRepositories Flux reads, and one ArtifactGenerator per environment that lays the chart beside the deploy repo's app names |
| `flux/generator.yaml` | One HelmRelease per environment: each app's namespace, its RoleBinding and its Kustomization |
| `namespaces/` | The chart those HelmReleases render, **in your platform repo**: the same chart Argo CD's set-up renders |

The files are in `deploy/gitops` in Canyon's source. The reference files in `deploy/gitops` are under
the Zero-Clause BSD licence (0BSD, `deploy/gitops/LICENSE`), not Canyon's: copy them, change them and
share them freely. They also come in `canyon-deploy.tar.gz` with every release. The placeholders are
Argo CD's ([Argo CD](/self-host/hand-off-argo-cd/)), and three more:

| Placeholder | What to put |
|---|---|
| `${REPO_URL}`, `${PREVIEW_BRANCH}`, `${PROD_BRANCH}`, `${ROOT}` | As in Canyon's form: the repo's address, the two branches and **Path in the repo** |
| `${READ_USER}`, `${READ_TOKEN}` | Flux's own read-only credential for the deploy repo |
| `${PLATFORM_REPO_URL}`, `${PLATFORM_REVISION}`, `${PLATFORM_PATH}` | Your platform repo, its branch, and where in it you put `namespaces/` |
| `${PLATFORM_READ_USER}`, `${PLATFORM_READ_TOKEN}` | Flux's read-only credential for your platform repo |
| `${INTERVAL}` | How often Flux reads the repos and reconciles: `1m` is a good default |

## What to install

Flux's own install, plus these. Each one is cluster-wide.

**Required.** Without any of them, the set-up doesn't work or isn't bounded:

- **source-watcher**, which runs the ArtifactGenerators. It is in Flux's `install.yaml`, but
  `flux install` and `flux bootstrap` add it only when asked:
  `--components-extra=source-watcher`.
- **helm-controller `--feature-gates=ExternalArtifact=true`**, so a HelmRelease can render a chart
  from an ArtifactGenerator's artifact. It is off by default.
- **kustomize-controller `--no-remote-bases=true`**, so a kustomization can't pull in a remote git base
  at sync time. It does not stop an `http(s)` file named in a kustomization's `resources:`:
  kustomize-controller 1.9.6 fetches and applies that, which Canyon's CI shows. What keeps the deploy
  repo's writer from either is the sync below, which leaves every kustomization file in the deploy repo
  out. Keep both.

**Hardening.** Canyon's bounds don't rely on them: the admission policy pins every reference and
account the generator writes. Each one also breaks things that worked before, so check your cluster
first:

- **`--no-cross-namespace-refs=true`** on kustomize-controller, helm-controller and source-watcher: no
  object refers to a source or a Secret in another namespace. It breaks any existing object of yours
  that does.
- **`--default-service-account=default`** on kustomize-controller and helm-controller: an object that
  names no account acts as its namespace's `default`, never as the controller. It breaks every
  existing Kustomization and HelmRelease without `serviceAccountName`, the `flux-system` Kustomization
  `flux bootstrap` made among them, unless you patch that one to `serviceAccountName:
  kustomize-controller`, as Flux's multi-tenancy guide shows.

## The namespaces Flux works in

`flux/namespaces.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: canyon-sync
  labels:
    pod-security.kubernetes.io/enforce: restricted
---
apiVersion: v1
kind: Namespace
metadata:
  name: canyon-namespaces
  labels:
    pod-security.kubernetes.io/enforce: restricted
---
# At most this many app environments' Kustomizations: a flood of canyon.json paths on a branch fails the generator's
# upgrade, which removes what it made (cleanupOnFail), instead of loading kustomize-controller. Size it to your apps.
apiVersion: v1
kind: ResourceQuota
metadata:
  name: canyon-sync
  namespace: canyon-sync
spec:
  hard:
    count/kustomizations.kustomize.toolkit.fluxcd.io: "500"
```

- **`canyon-sync`** holds the deploy repo's sync: its two GitRepositories and their read Secret, the
  sync accounts `canyon-preview` and `canyon-prod`, and each app's Kustomization. kustomize-controller
  acts as a Kustomization's account **in the Kustomization's own namespace**, and `flux-system`'s
  controllers are cluster-admin. Here, the strongest account a Kustomization could name is the other
  environment's sync account, and the admission policy refuses that too.
- **`canyon-namespaces`** holds Helm's records of the two namespaces releases, apart from
  `flux-system`.
- Neither name ends in `-preview` or `-prod`, so neither is an app namespace to the ClusterSecretStore
  or the Services policy.
- **The quota** caps the app Kustomizations at 500. Size it to your apps. Past it, a flood of
  `canyon.json` paths on a branch fails that environment's upgrade, and the upgrade removes what it
  made, instead of loading kustomize-controller.

## Who Flux acts as

`flux/rbac.yaml`:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: canyon-preview
  namespace: canyon-sync
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: canyon-prod
  namespace: canyon-sync
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: canyon-deploy
rules:
  - apiGroups: [apps]
    resources: [deployments]
    verbs: ["*"]
  - apiGroups: [""]
    resources: [services, configmaps]
    verbs: ["*"]
  - apiGroups: [networking.k8s.io]
    resources: [networkpolicies]
    verbs: ["*"]
  - apiGroups: [external-secrets.io]
    resources: [externalsecrets]
    verbs: ["*"]
  - apiGroups: [generators.external-secrets.io]
    resources: [passwords]
    verbs: ["*"]
  - apiGroups: [gateway.networking.k8s.io]
    resources: [httproutes]
    verbs: ["*"]
  - apiGroups: [postgresql.cnpg.io]
    resources: [clusters]
    verbs: ["*"]
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: canyon-namespaces
  namespace: flux-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: canyon-namespaces
rules:
  - apiGroups: [""]
    resources: [namespaces]
    verbs: [get, create, update, patch, delete]
  - apiGroups: [rbac.authorization.k8s.io]
    resources: [rolebindings]
    verbs: [get, create, update, patch, delete]
  - apiGroups: [rbac.authorization.k8s.io]
    resources: [clusterroles]
    resourceNames: [canyon-deploy]
    verbs: [bind]
  - apiGroups: [admissionregistration.k8s.io]
    resources: [validatingadmissionpolicies, validatingadmissionpolicybindings]
    resourceNames: [canyon-namespaces]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: canyon-namespaces
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: canyon-namespaces
subjects:
  - kind: ServiceAccount
    name: canyon-namespaces
    namespace: flux-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: canyon-namespaces
  namespace: canyon-sync
rules:
  - apiGroups: [kustomize.toolkit.fluxcd.io]
    resources: [kustomizations]
    verbs: [get, create, update, patch, delete]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: canyon-namespaces
  namespace: canyon-sync
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: canyon-namespaces
subjects:
  - kind: ServiceAccount
    name: canyon-namespaces
    namespace: flux-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: canyon-namespaces
  namespace: canyon-namespaces
rules:
  - apiGroups: [""]
    resources: [secrets]
    verbs: [get, list, create, update, patch, delete]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: canyon-namespaces
  namespace: canyon-namespaces
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: canyon-namespaces
subjects:
  - kind: ServiceAccount
    name: canyon-namespaces
    namespace: flux-system
```

- **The sync accounts** hold `canyon-deploy`: exactly the kinds the
  [AppProjects](/self-host/hand-off-argo-cd/#the-appprojects) list, with no subresource, so
  `services/status` stays ungranted. They are bound only by the RoleBinding the generator makes in each
  app's namespace, never cluster-wide, so a file that names another namespace is refused, and nothing
  they apply can make or relabel a Namespace. Add the kinds your own templates make to `canyon-deploy`,
  and nothing else.
- **The generator's account**, `canyon-namespaces`, may make, change and delete namespaces and
  RoleBindings, bind `canyon-deploy`, write Kustomizations in `canyon-sync` and its Helm records in
  `canyon-namespaces`, and read the admission policy that bounds it. It holds nothing in
  `flux-system`.

Kubernetes RBAC can't scope a create by name, and Canyon's namespaces come and go with its apps. By
RBAC alone, the generator's account could make, relabel or delete any namespace (`kube-system`,
`flux-system`), bind `canyon-deploy` in any namespace for any account, delete any RoleBinding in the
cluster, and read any namespace's or RoleBinding's metadata. A RoleBinding in `kube-system` plus a
Kustomization into it would make it cluster-admin. **That is why the admission policy below is not
optional.**

## The admission policy on the generator

`flux/admission.yaml`, once per cluster. It needs Kubernetes 1.30 or later, where
ValidatingAdmissionPolicy is GA:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: canyon-namespaces
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: [""]
        apiVersions: ["*"]
        operations: ["CREATE", "UPDATE", "DELETE"]
        resources: ["namespaces"]
      - apiGroups: ["rbac.authorization.k8s.io"]
        apiVersions: ["*"]
        operations: ["CREATE", "UPDATE", "DELETE"]
        resources: ["rolebindings"]
      - apiGroups: ["kustomize.toolkit.fluxcd.io"]
        apiVersions: ["*"]
        operations: ["CREATE", "UPDATE", "DELETE"]
        resources: ["kustomizations"]
  matchConditions:
    - name: canyon-namespaces-generator
      expression: request.userInfo.username == 'system:serviceaccount:flux-system:canyon-namespaces'
  variables:
    - name: res
      expression: request.resource.resource
    - name: obj
      expression: "request.operation == 'DELETE' ? oldObject : object"
    # The app namespace a request is about: the Namespace itself, the RoleBinding's namespace, the Kustomization's name.
    - name: name
      expression: "variables.res == 'rolebindings' ? request.namespace : variables.obj.metadata.name"
    - name: canyonName
      expression: >-
        variables.name.matches('^canyon-[a-z0-9]([-a-z0-9]*[a-z0-9])?-(preview|prod)$') && size(variables.name) <= 63
    - name: env
      expression: "variables.name.endsWith('-prod') ? 'prod' : 'preview'"
    - name: app
      expression: >-
        variables.canyonName ? variables.name.substring(7, size(variables.name) - size(variables.env) - 1) : ''
    - name: labelKeys
      expression: >-
        ['pod-security.kubernetes.io/enforce', 'canyon.dev/managed', 'canyon.dev/env', 'kubernetes.io/metadata.name',
        'app.kubernetes.io/managed-by', 'helm.toolkit.fluxcd.io/name', 'helm.toolkit.fluxcd.io/namespace']
    - name: annotationKeys
      expression: "['meta.helm.sh/release-name', 'meta.helm.sh/release-namespace']"
    - name: specKeys
      expression: >-
        ['interval', 'retryInterval', 'timeout', 'sourceRef', 'path', 'prune', 'wait', 'force', 'targetNamespace',
        'serviceAccountName', 'deletionPolicy']
  validations:
    - expression: >-
        variables.res != 'namespaces' || request.operation == 'CREATE' ||
        (variables.canyonName && has(oldObject.metadata.labels) && 'canyon.dev/managed' in oldObject.metadata.labels &&
        oldObject.metadata.labels['canyon.dev/managed'] == 'true')
      reason: Forbidden
      message: "Canyon's namespaces generator changes or deletes only Canyon's own namespaces:
        canyon-<app>-<preview|prod>, already labelled canyon.dev/managed."
    - expression: >-
        variables.res != 'namespaces' || request.operation == 'DELETE' ||
        (variables.canyonName && has(object.metadata.labels) &&
        'pod-security.kubernetes.io/enforce' in object.metadata.labels &&
        object.metadata.labels['pod-security.kubernetes.io/enforce'] == 'baseline' &&
        'canyon.dev/managed' in object.metadata.labels && object.metadata.labels['canyon.dev/managed'] == 'true' &&
        'canyon.dev/env' in object.metadata.labels && object.metadata.labels['canyon.dev/env'] == variables.env)
      reason: Forbidden
      message: "Canyon's namespaces generator makes namespaces named canyon-<app>-<preview|prod> alone, labelled Pod
        Security baseline, canyon.dev/managed \"true\" and canyon.dev/env their own environment."
    - expression: >-
        variables.res != 'namespaces' || request.operation == 'DELETE' ||
        ((!has(object.metadata.labels) || object.metadata.labels.all(k, k in variables.labelKeys ||
        (!k.startsWith('pod-security.kubernetes.io/') && !k.startsWith('canyon.dev/') &&
        request.operation == 'UPDATE' &&
        has(oldObject.metadata.labels) && k in oldObject.metadata.labels &&
        oldObject.metadata.labels[k] == object.metadata.labels[k]))) &&
        (!has(object.metadata.annotations) || object.metadata.annotations.all(k, k in variables.annotationKeys ||
        (request.operation == 'UPDATE' && has(oldObject.metadata.annotations) && k in oldObject.metadata.annotations &&
        oldObject.metadata.annotations[k] == object.metadata.annotations[k]))))
      reason: Forbidden
      message: "Canyon's namespaces generator sets no label or annotation on a namespace but Canyon's three and
        those Helm and Flux put on what they apply."
    - expression: >-
        variables.res != 'rolebindings' ||
        (namespaceObject != null &&
        namespaceObject.metadata.name.matches('^canyon-[a-z0-9]([-a-z0-9]*[a-z0-9])?-(preview|prod)$') &&
        has(namespaceObject.metadata.labels) && 'canyon.dev/managed' in namespaceObject.metadata.labels &&
        namespaceObject.metadata.labels['canyon.dev/managed'] == 'true' &&
        'canyon.dev/env' in namespaceObject.metadata.labels &&
        namespaceObject.metadata.labels['canyon.dev/env'] ==
        (namespaceObject.metadata.name.endsWith('-prod') ? 'prod' : 'preview'))
      reason: Forbidden
      message: "Canyon's namespaces generator makes, changes and deletes RoleBindings in Canyon's own app namespaces
        alone."
    - expression: >-
        variables.res != 'rolebindings' || (variables.obj.metadata.name == 'canyon-deploy' &&
        (request.operation == 'DELETE' || (object.roleRef.apiGroup == 'rbac.authorization.k8s.io' &&
        object.roleRef.kind == 'ClusterRole' && object.roleRef.name == 'canyon-deploy')))
      reason: Forbidden
      message: "Canyon's namespaces generator binds the ClusterRole canyon-deploy alone, in a RoleBinding named
        canyon-deploy."
    - expression: >-
        variables.res != 'rolebindings' || request.operation == 'DELETE' ||
        (has(object.subjects) && size(object.subjects) == 1 && object.subjects[0].kind == 'ServiceAccount' &&
        object.subjects[0].name == 'canyon-' + variables.env && has(object.subjects[0].namespace) &&
        object.subjects[0].namespace == 'canyon-sync')
      reason: Forbidden
      message: "Canyon's namespaces generator binds canyon-deploy for the namespace's own environment's sync account
        alone, canyon-sync/canyon-<env>."
    - expression: >-
        variables.res != 'kustomizations' || (request.namespace == 'canyon-sync' && variables.canyonName)
      reason: Forbidden
      message: "Canyon's namespaces generator makes, changes and deletes Kustomizations in canyon-sync alone, each
        named as its app namespace, canyon-<app>-<preview|prod>."
    - expression: >-
        variables.res != 'kustomizations' || request.operation == 'DELETE' ||
        object.spec.all(k, k in variables.specKeys)
      reason: Forbidden
      message: "Canyon's namespaces generator sets no Kustomization field but interval, retryInterval, timeout,
        sourceRef, path, prune, wait, force, targetNamespace, serviceAccountName and deletionPolicy."
    - expression: >-
        variables.res != 'kustomizations' || request.operation == 'DELETE' ||
        (has(object.spec.targetNamespace) && object.spec.targetNamespace == variables.name &&
        has(object.spec.serviceAccountName) && object.spec.serviceAccountName == 'canyon-' + variables.env &&
        object.spec.sourceRef.all(k, k in ['kind', 'name']) && object.spec.sourceRef.kind == 'GitRepository' &&
        object.spec.sourceRef.name == 'canyon-' + variables.env &&
        object.spec.path == './${ROOT}/' + variables.app + '/' + variables.env &&
        object.spec.prune == true && has(object.spec.deletionPolicy) && object.spec.deletionPolicy == 'Orphan' &&
        (!has(object.spec.force) || object.spec.force == false))
      reason: Forbidden
      message: "Canyon's namespaces generator pins each Kustomization: its namespace as targetNamespace, its
        environment's sync account and GitRepository, its app's directory, prune with deletionPolicy Orphan, and
        force false."
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: canyon-namespaces
spec:
  policyName: canyon-namespaces
  validationActions: [Deny]
```

It does for Flux what Argo CD's `canyon-namespaces` AppProject does for Argo CD's generator, and more.
Every request the generator's account makes is held to Canyon's own:

- **Namespaces** named `canyon-<app>-<preview|prod>`, with exactly Canyon's labels. It changes or
  deletes a namespace only when it is already Canyon's, so it can never take over another team's.
- **RoleBindings** of `canyon-deploy`, for the namespace's own environment's sync account, in those
  namespaces alone.
- **Kustomizations** in `canyon-sync` alone, every reference and account pinned, and no other field admitted, so none Flux adds
  later either.

It fails closed: an expression that can't be evaluated denies. The chart checks that the policy and its
binding are on the cluster before it renders anything, so a cluster without them makes no namespace.
Labels and annotations Helm and Flux put on what they apply are allowed by name. On a cluster where
another controller adds its own to namespaces, the policy keeps whatever a namespace already has.

## The read credentials

`flux/repository.yaml`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: canyon-deploy-repo
  namespace: canyon-sync
stringData:
  username: ${READ_USER}
  password: ${READ_TOKEN}
---
apiVersion: v1
kind: Secret
metadata:
  name: canyon-deploy-repo
  namespace: flux-system
stringData:
  username: ${READ_USER}
  password: ${READ_TOKEN}
---
apiVersion: v1
kind: Secret
metadata:
  name: canyon-platform-repo
  namespace: flux-system
stringData:
  username: ${PLATFORM_READ_USER}
  password: ${PLATFORM_READ_TOKEN}
```

Flux's own read-only credentials, never Canyon's token, which can write. On GitHub, a fine-grained token
with Contents read; on Gitea or Forgejo, an access token with `read:repository`; on GitLab, a project
access token with the Reporter role and `read_repository`, or a deploy token with `read_repository` alone.
If a private CA signs a
repo's address, add the CA's certificate to its Secret as `ca.crt`.

## The sources and the generator

`flux/sources.yaml`:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: canyon-preview
  namespace: canyon-sync
spec:
  url: ${REPO_URL}
  ref:
    branch: ${PREVIEW_BRANCH}
  interval: ${INTERVAL}
  secretRef:
    name: canyon-deploy-repo
  ignore: |
    /*
    !/${ROOT}/*/preview/
    **/canyon.json
    **/kustomization.yaml
    **/kustomization.yml
    **/Kustomization
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: canyon-prod
  namespace: canyon-sync
spec:
  url: ${REPO_URL}
  ref:
    branch: ${PROD_BRANCH}
  interval: ${INTERVAL}
  secretRef:
    name: canyon-deploy-repo
  ignore: |
    /*
    !/${ROOT}/*/prod/
    **/canyon.json
    **/kustomization.yaml
    **/kustomization.yml
    **/Kustomization
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: canyon-preview-names
  namespace: flux-system
spec:
  url: ${REPO_URL}
  ref:
    branch: ${PREVIEW_BRANCH}
  interval: ${INTERVAL}
  secretRef:
    name: canyon-deploy-repo
  ignore: |
    /*
    !/${ROOT}/*/preview/canyon.json
    !/${ROOT}/.canyon/route.yaml
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: canyon-prod-names
  namespace: flux-system
spec:
  url: ${REPO_URL}
  ref:
    branch: ${PROD_BRANCH}
  interval: ${INTERVAL}
  secretRef:
    name: canyon-deploy-repo
  ignore: |
    /*
    !/${ROOT}/*/prod/canyon.json
    !/${ROOT}/.canyon/route.yaml
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: canyon-platform
  namespace: flux-system
spec:
  url: ${PLATFORM_REPO_URL}
  ref:
    branch: ${PLATFORM_REVISION}
  interval: ${INTERVAL}
  secretRef:
    name: canyon-platform-repo
---
apiVersion: source.extensions.fluxcd.io/v1beta1
kind: ArtifactGenerator
metadata:
  name: canyon-namespaces-preview
  namespace: flux-system
spec:
  sources:
    - alias: platform
      kind: GitRepository
      name: canyon-platform
    - alias: names
      kind: GitRepository
      name: canyon-preview-names
  artifacts:
    - name: canyon-namespaces-preview
      copy:
        - from: "@platform/${PLATFORM_PATH}/**"
          to: "@artifact/canyon-namespace/"
        - from: "@names/"
          to: "@artifact/canyon-namespace/deploy/"
---
apiVersion: source.extensions.fluxcd.io/v1beta1
kind: ArtifactGenerator
metadata:
  name: canyon-namespaces-prod
  namespace: flux-system
spec:
  sources:
    - alias: platform
      kind: GitRepository
      name: canyon-platform
    - alias: names
      kind: GitRepository
      name: canyon-prod-names
  artifacts:
    - name: canyon-namespaces-prod
      copy:
        - from: "@platform/${PLATFORM_PATH}/**"
          to: "@artifact/canyon-namespace/"
        - from: "@names/"
          to: "@artifact/canyon-namespace/deploy/"
```

- **`canyon-preview` and `canyon-prod`** read each environment's branch for the apps' Kustomizations.
  Each keeps `<root>/<app>/<env>/` for its own environment alone, so a file on the preview branch can
  never reach production. It leaves out every `canyon.json` and **every kustomization file**:
  kustomize-controller builds the manifests Canyon commits as they are, as Argo CD's directory source
  does, and a `kustomization.yaml` committed to the deploy repo is never run.
- **`canyon-preview-names` and `canyon-prod-names`** read the same branches for the generator, and
  keep two kinds of file: their own environment's `canyon.json` files and your route template,
  `<root>/.canyon/route.yaml`.
- **One ArtifactGenerator per environment** lays out the chart from your platform repo as
  `canyon-namespace/`, and copies its environment's names under `canyon-namespace/deploy/`. The chart
  reads their paths, never their contents. A problem on the preview side never stalls production's.

**Keep `<root>/.canyon/route.yaml` on both branches.** It is the generator's sentinel. While an
environment's artifact lacks it, the chart refuses to render, and nothing is made or removed in that
environment. The HelmRelease says why. Canyon refuses every release without that template too, so a
working deploy repo always has it. Because it comes through the same ignore, copy and root as the
apps' names, a mistake in any of them loses the sentinel along with the apps. The release then stops,
instead of reading the environment as empty and deleting every app namespace with its data. An
environment whose last app is deleted still has its sentinel, so its last namespace is removed, as
under Argo CD.

## The namespaces release

`flux/generator.yaml`:

```yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: canyon-namespaces-preview
  namespace: flux-system
spec:
  releaseName: canyon-namespaces-preview
  chartRef:
    kind: ExternalArtifact
    name: canyon-namespaces-preview
  serviceAccountName: canyon-namespaces
  targetNamespace: canyon-namespaces
  storageNamespace: canyon-namespaces
  interval: ${INTERVAL}
  maxHistory: 3
  install:
    disableWait: true
    disableTakeOwnership: true
    strategy:
      name: RetryOnFailure
      retryInterval: 1m
  upgrade:
    disableWait: true
    disableTakeOwnership: true
    cleanupOnFail: true
    strategy:
      name: RetryOnFailure
      retryInterval: 1m
  driftDetection:
    mode: enabled
  values:
    env: preview
    root: "${ROOT}"
    interval: "${INTERVAL}"
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: canyon-namespaces-prod
  namespace: flux-system
spec:
  releaseName: canyon-namespaces-prod
  chartRef:
    kind: ExternalArtifact
    name: canyon-namespaces-prod
  serviceAccountName: canyon-namespaces
  targetNamespace: canyon-namespaces
  storageNamespace: canyon-namespaces
  interval: ${INTERVAL}
  maxHistory: 3
  install:
    disableWait: true
    disableTakeOwnership: true
    strategy:
      name: RetryOnFailure
      retryInterval: 1m
  upgrade:
    disableWait: true
    disableTakeOwnership: true
    cleanupOnFail: true
    strategy:
      name: RetryOnFailure
      retryInterval: 1m
  driftDetection:
    mode: enabled
  values:
    env: prod
    root: "${ROOT}"
    interval: "${INTERVAL}"
```

- **For each `canyon.json`** on the environment's branch, the release makes the namespace
  `canyon-<app>-<env>`, the RoleBinding that lets the environment's sync account apply there, and the
  app's Kustomization. When an app's directory goes, Helm's next upgrade deletes all three: the
  namespace with everything in it, the database and its data among them.
- **`disableWait`**: one app's Kustomization that is not Ready never holds another app's namespace.
- **`disableTakeOwnership`**: a path never takes over a namespace or Kustomization someone else made.
  `canyon-*-preview` and `canyon-*-prod` names are Canyon's, cluster-wide: don't use them for anything
  else.
- **`RetryOnFailure`**: a failed upgrade is retried as it is, never rolled back or uninstalled, so a
  failure deletes nothing. **`cleanupOnFail`** removes only what the failed attempt itself made, which
  holds no data yet.
- **The release upgrades on every deploy**, since `canyon.json` changes with each release. That is
  expected; `maxHistory` keeps Helm's records few.

## The chart's Flux mode

Copy `deploy/gitops/namespaces` into your platform repo at `${PLATFORM_PATH}`, all of it. It is the
chart Argo CD's set-up renders; under Flux the HelmRelease sets `root` instead of `app`, and the chart
reads the app names from the paths beside it. `templates/namespace.yaml` makes each namespace:

```yaml
{{- /*
One app environment's namespace, the only object either mode makes from the deploy repo's path: canyon.namespace
checks app and env, and makes canyon-<app>-<env>, Pod Security baseline, labelled canyon.dev/managed and
canyon.dev/env. Argo CD's mode (app set) renders it once; Flux's (root set) includes canyon.flux, defined in flux.yaml,
which renders it once per app it finds: a copy of the chart without flux.yaml then fails, never renders nothing.
*/ -}}
{{- define "canyon.namespace" }}
{{- $app := toString .app }}
{{- $env := toString .env }}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $app) }}
{{- fail "app: a DNS label, the app's directory in the deploy repo" }}
{{- end }}
{{- if not (has $env (list "preview" "prod")) }}
{{- fail "env: preview or prod, set by the generator" }}
{{- end }}
{{- $name := printf "canyon-%s-%s" $app $env }}
{{- if gt (len $name) 63 }}
{{- fail "canyon-<app>-<env> is longer than 63 characters" }}
{{- end }}
apiVersion: v1
kind: Namespace
metadata:
  name: {{ $name | quote }}
  labels:
    pod-security.kubernetes.io/enforce: baseline
    canyon.dev/managed: "true"
    canyon.dev/env: {{ $env | quote }}
{{- end }}
{{- /* Empty: flux.yaml includes it under Flux's root, so a copy of the chart without this file fails to render. */ -}}
{{- define "canyon.namespace.chart" }}{{- end }}
{{- if eq (empty .Values.app) (empty .Values.root) }}
{{- fail "set app (Argo CD's ApplicationSet) or root (Flux's HelmRelease), one of them" }}
{{- end }}
{{- if .Values.app }}
{{- include "canyon.namespace" (dict "app" .Values.app "env" .Values.env) }}
{{- end }}
{{- if .Values.root }}
{{- include "canyon.flux" . }}
{{- end }}
```

`templates/flux.yaml` makes each app's three objects:

```yaml
{{- /*
Flux's mode (deploy/gitops/flux/generator.yaml sets root, and namespace.yaml includes canyon.flux, so a copy of the
chart without this file fails to render instead of rendering nothing): one namespace per app the deploy repo's env
branch holds, from the paths alone. The ArtifactGenerator copies that branch's <root>/<app>/<env>/canyon.json files
and <root>/.canyon/route.yaml under deploy/ beside this chart; their contents are never read here, only their paths. For
each app: the Namespace (canyon.namespace), a RoleBinding of canyon-deploy for the environment's sync account in it,
and the Kustomization in canyon-sync that applies the app's directory into it, each held by the admission policy
canyon-namespaces (deploy/gitops/flux/admission.yaml).

It fails, changing nothing, unless the policy and its binding are on the cluster, and unless the sentinel
<root>/.canyon/route.yaml is among the files: every working deploy repo has it (Canyon refuses a release without
it), and it comes through the same ignore, copy and root as the apps, so a setting that loses the apps loses it too,
and a misconfiguration never renders an empty environment that would delete every namespace with its data.
*/ -}}
{{- if .Values.root }}
{{- /* namespace.yaml includes canyon.flux, and this needs namespace.yaml back: either file missing fails the render. */ -}}
{{- $_ := include "canyon.namespace.chart" . }}
{{- end }}
{{- define "canyon.flux" }}
{{- $root := toString .Values.root }}
{{- $env := toString .Values.env }}
{{- $interval := toString .Values.interval }}
{{- if not (regexMatch "^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$" $root) }}
{{- fail "root: the deploy repo's path root as Canyon's form takes it, such as apps" }}
{{- end }}
{{- if not (has $env (list "preview" "prod")) }}
{{- fail "env: preview or prod, set by the generator" }}
{{- end }}
{{- if not (regexMatch "^([0-9]+(\\.[0-9]+)?(ms|s|m|h))+$" $interval) }}
{{- fail "interval: a duration, such as 1m" }}
{{- end }}
{{- $policy := lookup "admissionregistration.k8s.io/v1" "ValidatingAdmissionPolicy" "" "canyon-namespaces" }}
{{- $binding := lookup "admissionregistration.k8s.io/v1" "ValidatingAdmissionPolicyBinding" "" "canyon-namespaces" }}
{{- if not (and (eq (dig "spec" "failurePolicy" "" $policy) "Fail") (eq (dig "spec" "policyName" "" $binding) "canyon-namespaces") (has "Deny" (dig "spec" "validationActions" (list) $binding))) }}
{{- fail "the admission policy canyon-namespaces and its binding (deploy/gitops/flux/admission.yaml) are not on the cluster: without them this generator could bind into any namespace, so it makes nothing" }}
{{- end }}
{{- $deploy := printf "deploy/%s/" $root }}
{{- $sentinel := false }}
{{- range $path, $_ := .Files.Glob (printf "%s.canyon/route.yaml" $deploy) }}
{{- $sentinel = true }}
{{- end }}
{{- if not $sentinel }}
{{- fail (printf "%s.canyon/route.yaml is not in the generator's artifact: the deploy repo's templates are missing from the %s branch, or the names GitRepository's ignore, the ArtifactGenerator's copy and this release's root disagree. Nothing is made or removed until it is there." $deploy $env) }}
{{- end }}
{{- range $path, $_ := .Files.Glob (printf "%s*/%s/canyon.json" $deploy $env) }}
{{- $parts := splitList "/" (trimPrefix $deploy $path) }}
{{- if or (not (hasPrefix $deploy $path)) (ne (len $parts) 3) (ne (index $parts 1) $env) (ne (index $parts 2) "canyon.json") }}
{{- fail (printf "%q is not %s<app>/%s/canyon.json" $path $deploy $env) }}
{{- end }}
{{- $app := index $parts 0 }}
---
{{- include "canyon.namespace" (dict "app" $app "env" $env) }}
{{- $name := printf "canyon-%s-%s" $app $env }}
{{- $sync := printf "canyon-%s" $env }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: canyon-deploy
  namespace: {{ $name | quote }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: canyon-deploy
subjects:
  - kind: ServiceAccount
    name: {{ $sync | quote }}
    namespace: canyon-sync
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: {{ $name | quote }}
  namespace: canyon-sync
spec:
  sourceRef:
    kind: GitRepository
    name: {{ $sync | quote }}
  path: {{ printf "./%s/%s/%s" $root $app $env | quote }}
  targetNamespace: {{ $name | quote }}
  serviceAccountName: {{ $sync | quote }}
  interval: {{ $interval | quote }}
  prune: true
  wait: false
  deletionPolicy: Orphan
{{- end }}
{{- end }}
```

It checks `root` as Canyon's form does and every path's shape, and each app name as a DNS label of at
most 63 characters with its prefix and environment. One bad directory name fails the whole render, so
nothing that isn't a name ever becomes YAML.

## Each app's Kustomization

The chart makes one per app and environment, in `canyon-sync`, named as the app's namespace:

- **`targetNamespace`** is the app's own namespace, so every object of the app's directory lands there,
  whatever namespace a file names.
- **`serviceAccountName`** is its environment's sync account, and **`sourceRef`** its environment's
  GitRepository. A namespace ending `-prod` only ever syncs the production branch.
- **`prune: true`**: a file Canyon removes is deleted from the cluster. An undeploy keeps `canyon.json`
  and `resources/`, so the database and its data stay.
- **`deletionPolicy: Orphan`**: when the app is deleted, the Kustomization goes without pruning, and
  deleting the namespace takes what is in it. Nothing has to act as the sync account in a namespace
  whose RoleBinding is already gone.
- **`wait: false`**: Flux doesn't hold one app's sync on its health. Canyon learns an app is up from
  its pod's check-in, not from Flux.

## What the deploy repo's writer can do under Flux

Whoever can write the deploy repo, Canyon's token or a person, is bounded as under Argo CD: the kinds
`canyon-deploy` lists, in the app's own namespace, from its own environment's branch, never a
cluster-scoped object or a Namespace. Kustomization files are left out of the sync, so the writer gets
no kustomize generator, patch or remote base. Two things remain, each with an Argo CD counterpart:

- The annotations `kustomize.toolkit.fluxcd.io/prune: disabled` and `kustomize.toolkit.fluxcd.io/ssa:
  Ignore` keep an object through an undeploy, as Argo CD's `Prune=false` does. Canyon's deny-all stops
  such a pod admitting anyone
  ([When an environment is stopped](/self-host/hand-off/#when-an-environment-is-stopped)).
- **Deleting `<root>/.canyon/route.yaml`** from a branch stops that environment's namespaces until it
  is back (see the failure modes below). Canyon's releases stop with it, as they do under Argo CD.
  Put `<root>/.canyon/` under code owners.

## When it fails

Each of these fails static: nothing is deleted, and the environment's namespaces stay as they are
until it is fixed.

- **source-watcher down or restarted, or a names repo that can't be fetched**: the last artifact is
  kept, or the release waits for its source. New apps get no namespace, deleted apps keep theirs, and
  labels changed by hand aren't restored until it is back.
- **One bad directory name on a branch**, such as `apps/Bad_App/preview/canyon.json`, **or a namespace
  still terminating while its app is added again**: that environment's upgrades fail until it is
  fixed. New namespaces, the deletes of removed apps (their data stays until then) and drift
  correction all wait. `RetryOnFailure` recovers by itself once the cause is gone.
- **`<root>/.canyon/route.yaml` deleted from a branch**, by mistake or by anyone who can write the
  deploy repo: the same as a bad name, for that environment. Restore it.
- **`canyon.json` files too large for Helm's release record**, which holds the chart's files and is at
  most 1 MiB: the same as a bad name, for that environment. Canyon's own `canyon.json` is a few hundred
  bytes, so only a writer's own files reach this.
- **The Kustomization quota reached.** One ResourceQuota in `canyon-sync` counts both environments' app
  Kustomizations, so a preview branch with enough `canyon.json` paths, pushed with no review, can fill
  it. Production's upgrades that add an app then fail with `exceeded quota` until preview has fewer
  apps; production's existing apps still deploy. Size the quota with room for both environments, raise
  `count/kustomizations.kustomize.toolkit.fluxcd.io` in your copy of `flux/namespaces.yaml` when you
  near it, and alert when its `used` nears `hard`.

```sh
kubectl -n flux-system get helmreleases,artifactgenerators
kubectl -n flux-system describe helmrelease canyon-namespaces-<env>
kubectl -n canyon-sync get kustomizations
```

**What deletes every app namespace of an environment, with its data.** Each of these does, so never do
one while apps run:

- deleting a namespaces HelmRelease;
- changing its `targetNamespace` or `storageNamespace`: Flux uninstalls the release and installs it
  again;
- renaming the release;
- moving the deploy repo, or changing the root, before Canyon has committed the apps there: the new
  place has the templates and no app yet;
- pointing a release at the other environment's names, through its `values.env` or its names
  GitRepository's branch: both branches carry the sentinel, so it finds no app of its environment;
- putting a different chart at `${PLATFORM_PATH}`: one that renders no Kustomization, Namespace or
  RoleBinding for your apps, whatever its version, reads as every app gone.

## Moving from the hand-made set-up

If you made each app's namespace and RoleBinding by hand under Canyon's earlier Flux reference:

1. **First**, turn pruning off where a removal could delete something:
   - Set `prune: false` on the old per-environment Kustomizations `canyon-preview` and `canyon-prod` in
     `flux-system`.
   - Do the same on the Kustomization of your platform repo that applies the namespaces and RoleBindings
     you made, or annotate each of those objects `kustomize.toolkit.fluxcd.io/prune: disabled`.
2. Apply the new files with `disableTakeOwnership: false` on both HelmReleases, for one reconcile, so
   they take over the namespaces and the `canyon-deploy` RoleBindings you made. Your namespaces already
   carry `canyon.dev/managed: "true"`, which the admission policy needs to let the generator change them.
3. Set `disableTakeOwnership` back to `true`.
4. Remove the namespaces and RoleBindings from your platform repo, with its Kustomization's pruning
   still off. They are the namespaces release's now: never delete them by hand, which would delete
   Helm's. Turn that pruning back on only once the Kustomization's `status.inventory` no longer lists
   them.
5. Delete the old Kustomizations and GitRepositories `canyon-preview` and `canyon-prod` in
   `flux-system`.

## What Flux can't tell Canyon

The reference set-up for Flux reports nothing to Canyon: Flux's alerts post Flux's own events, not the
report Canyon reads ([Reporting a failed sync](/self-host/hand-off-argo-cd/#reporting-a-failed-sync) is
Argo CD's). A release goes live at its pod's first check-in, and a sync that fails shows as no pod
checking in. At ten minutes the deploy says "Canyon committed v1.4 10 minutes ago and no pod has
checked in: look at your Argo CD or Flux sync", and at 30 it ends "No pod has checked in on v1.4 in 30
minutes. The release stays open: when a pod checks in, the environment goes live." Then:

```sh
kubectl get namespace canyon-<app>-<env>
kubectl -n flux-system get helmreleases
kubectl -n canyon-sync get kustomization canyon-<app>-<env>
kubectl get externalsecret -n canyon-<app>-<env>
kubectl get events -n canyon-<app>-<env>
```

[Troubleshooting](/self-host/hand-off/#troubleshooting) follows a release from git to the pod.

## Related

- [Your platform runs the apps](/self-host/hand-off/): hand-off, the deploy repo and what Canyon commits.
- [Argo CD](/self-host/hand-off-argo-cd/): the same set-up for Argo CD.
- [External Secrets](/self-host/hand-off-external-secrets/): the ClusterSecretStore and the fetch-only token.
