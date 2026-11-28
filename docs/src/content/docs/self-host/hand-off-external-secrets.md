---
title: External Secrets
description: Bring each release's access token into its namespace with External Secrets. The ClusterSecretStore to apply once, the fetch-only token, keeping templates away from other teams' secrets, rotation, and using another secret mechanism.
---

This page is for platform teams setting up Canyon's deploy repo
([Your deploy repo](/self-host/hand-off/#your-deploy-repo)). After reading it, you can apply
the store each release's access token comes through, keep it to Canyon's namespaces, rotate its
token, and replace it with your own mechanism if you don't run External Secrets.

The sidecar in each pod needs its release's **access token** to pull the app's access list
([How the sidecar gets access](/self-host/hand-off/#how-the-sidecar-gets-access)). Canyon never puts it
in git. Instead, each release's `externalsecret.yaml` asks External Secrets for it, and External
Secrets fetches it from Canyon with the **fetch-only token**, into the Secret
`<app>-<env>-access-<hex>` that the pod reads.

## The ClusterSecretStore

Apply it once per cluster, outside the deploy repo. It is `external-secrets/clustersecretstore.yaml` in
`deploy/gitops` in Canyon's source, and **Settings › App hosting › For your platform team** shows it
filled in for your Canyon. The reference files in `deploy/gitops` are under the Zero-Clause BSD licence (0BSD,
`deploy/gitops/LICENSE`), not Canyon's: copy them, change them and share them freely. They also come in
`canyon-deploy.tar.gz` with every release.

```yaml
apiVersion: external-secrets.io/v1
kind: ClusterSecretStore
metadata:
  name: ${STORE}
spec:
  conditions:
    - namespaceRegexes:
        - "^canyon-.+-(preview|prod)$"
  provider:
    webhook:
      url: "${CANYON_URL}/releases/{{ .remoteRef.key }}/access-token"
      method: GET
      headers:
        Authorization: "Bearer {{ .auth.token }}"
      result:
        jsonPath: "$.value"
      secrets:
        - name: auth
          secretRef:
            name: canyon-fetch-token
            namespace: external-secrets
${CA_BUNDLE}
```

| Placeholder | What to put |
|---|---|
| `${STORE}` | The **Secret store** you named in Canyon's form: `canyon` unless you chose another |
| `${CANYON_URL}` | Your Canyon's address, `CANYON_PUBLIC_URL` |
| `${CA_BUNDLE}` | Nothing, when a public CA signed Canyon's address. Otherwise the line `      caBundle: <base64 of the CA that signed Canyon's address>`, indented under `webhook`. **For your platform team** fills it in from **Your private certificate authority, if one signed Canyon's address (optional)** |

How it works: each release's ExternalSecret names the release as its key, so External Secrets calls
`GET <your Canyon>/releases/<release>/access-token` with the fetch-only token. Canyon answers
`{"name": "CANYON_ACCESS_TOKEN", "value": "…"}`, and the store keeps `$.value` as the Secret's
`CANYON_ACCESS_TOKEN`. External Secrets' controller reaches Canyon over `https`, so allow it out to
your Canyon's address if your cluster restricts egress.

## The fetch-only token

Canyon shows the fetch-only token once, when the deploy repo is first saved, in **Store these now**.
Put it in the Secret `canyon-fetch-token`, key `token`, in the `external-secrets` namespace, from your
own secret store, never from git. External Secrets' webhook provider reads only a Secret labelled
`external-secrets.io/type: webhook`. By hand, for example:

```sh
kubectl -n external-secrets create secret generic canyon-fetch-token --from-literal=token="$FETCH_TOKEN"
kubectl -n external-secrets label secret canyon-fetch-token external-secrets.io/type=webhook
```

You should see `secret/canyon-fetch-token created`, then `secret/canyon-fetch-token labeled`. Without
the label, every release's ExternalSecret reads `SecretSyncedError`: "secret does not contain needed
label 'external-secrets.io/type: webhook'".

It differs from the host token in what it opens:

| | Fetch-only token | Host token |
|---|---|---|
| Made | By the deploy repo's first save, or **Replace the fetch-only token** | By the deploy repo's first save, or **Replace the host token** in the repo's step. The webhook, if you save one, has a host token of its own, and either works |
| Fetches each release's access token | Yes | Yes |
| Reports on a release | No | Yes |
| Anything else | Nothing | Nothing |

So the token your cluster holds can fetch access tokens and do nothing else. The host token, which
can report, stays out of the cluster unless you send Canyon failure reports from Argo CD
([Reporting a failed sync](/self-host/hand-off-argo-cd/#reporting-a-failed-sync)).

## Keep it to Canyon's namespaces, and others out

`conditions.namespaceRegexes: ["^canyon-.+-(preview|prod)$"]` lets only Canyon's app namespaces,
`canyon-<app>-preview` and `canyon-<app>-prod`, read through Canyon's store, so nothing else in your
cluster can fetch access tokens with it. Canyon's own namespaces, such as `canyon-apps` and
`canyon-egress`, never match.

### What it still allows

The fetch-only token isn't bound to one app, and Canyon accepts the risk that leaves: any
ExternalSecret in an app namespace can fetch **any** app's newest access token through the store, by
naming that app's release. With it, it can pull that app's access list, its members' emails and roles,
and check in for it. Two things limit what that can do:

- **A check-in settles only what git already holds.** A release goes live only when its branch holds
  exactly the files Canyon committed, unedited, so a faked check-in at most claims live what git
  already says runs ([Deployed by check-in](/self-host/hand-off/#deployed-by-check-in)).
- **A stopped environment lets nobody in.** Once an environment is undeployed, held or deleted, its
  access list admits no one, whoever pulls it
  ([When an environment is stopped](/self-host/hand-off/#when-an-environment-is-stopped)).

What your platform team can do: keep who may create an ExternalSecret in an app namespace to Canyon's
deploy repo alone. The reference AppProjects already do, since only they deploy into
`canyon-*-preview` and `canyon-*-prod`, and only from the deploy repo; under Flux, so do the sync
accounts, bound only in those namespaces. Give no other AppProject, Flux Kustomization, service account
or person a way to create ExternalSecrets there, and protect the deploy
repo's branches ([Protecting the branches](/self-host/hand-off/#protecting-the-branches)).

### Keep other stores out

The other way matters as much: **every other ClusterSecretStore you have must exclude Canyon's app
namespaces.** A template in the deploy repo can make an ExternalSecret in an app's namespace, and
whoever edits the deploy repo is not necessarily allowed your other teams' secrets. Exclude them with
the label the namespaces chart puts on each app namespace:

```yaml
# On each of your other ClusterSecretStores.
spec:
  conditions:
    - namespaceSelector:
        matchExpressions:
          - key: canyon.dev/managed
            operator: DoesNotExist
```

Or list exactly the namespaces each store serves, with `conditions.namespaces`. Keep SecretStore out of
the AppProjects' kinds, so no template can make a store of its own. The reference `postgres.yaml`
needs no store: its Password generator makes the database's password inside the app's namespace.

**ClusterGenerators have no such conditions.** An ExternalSecret in any namespace can name one as its
`generatorRef`, and a ClusterGenerator's credentials may come from a Secret in any namespace, so an
ECR, Vault or other generator you define cluster-wide mints its credential for whoever writes the deploy
repo too. Don't run ClusterGenerators on a cluster where Canyon's apps run, or use namespaced
generators in your own namespaces instead.

## Rotating the fetch-only token

If the fetch-only token is lost or leaked, an admin chooses **Replace the fetch-only token** in
**Settings › App hosting**, or calls `POST /api/v1/platform-repo/fetch-token`. Canyon asks first:
"Replace the fetch-only token? The current one stops working at once: External Secrets can't fetch
access tokens until it has the new one." Then it shows the new one once.

Put it in `canyon-fetch-token` straight away:

```sh
kubectl -n external-secrets create secret generic canyon-fetch-token --from-literal=token="$FETCH_TOKEN" \
  --dry-run=client -o yaml | kubectl label --local -f - external-secrets.io/type=webhook -o yaml | kubectl apply -f -
```

You should see `secret/canyon-fetch-token configured`. Pods already running keep the access token they
have. Until the Secret holds the new token, a new release's ExternalSecret can't fetch its token, so
its pod doesn't start, and the deploy waits.

## When the token doesn't arrive

`kubectl get externalsecret -n canyon-<app>-<env>` shows each ExternalSecret's status;
`kubectl describe externalsecret -n canyon-<app>-<env>` says why one isn't `SecretSynced`:

- **`401` from Canyon**: the Secret `canyon-fetch-token` doesn't hold the current fetch-only token.
  Put the current one in, or replace it.
- **`404` from Canyon**: a newer release of the environment exists. The older ExternalSecret goes at
  the next sync, with the newer release's files.
- **A certificate error**: a private CA signed Canyon's address. Set **CA for the sign-in sidecar and
  External Secrets (optional)** in Canyon's form, and copy the store again from **For your platform
  team**, which then carries it as `caBundle`.
- **The store doesn't allow the namespace**: the namespace isn't `canyon-<app>-preview` or
  `canyon-<app>-prod`, or the store's `conditions` were changed.
- **No connection**: External Secrets' controller can't reach your Canyon's address.

## Using another secret mechanism

If your platform brings Secrets another way, such as Vault Agent or the Secrets Store CSI driver,
replace Canyon's ExternalSecret: commit `<root>/.canyon/externalsecret.yaml` to both branches, and
Canyon renders it in place of its own. It is a template like the others
([Templates](/self-host/hand-off/#templates)), and it must make the Secret `${secret}`, which is
`<app>-<env>-access-<hex>`, with the key `CANYON_ACCESS_TOKEN`, holding what
`GET /releases/${release}/access-token` answers with the fetch-only token. It is the only template that
may make that Secret. This one is the same as Canyon's own, a starting point:

```yaml
# <root>/.canyon/externalsecret.yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: ${secret}
spec:
  refreshInterval: 1h
  secretStoreRef:
    kind: ClusterSecretStore
    name: ${store}
  target:
    name: ${secret}
    creationPolicy: Owner
  data:
    - secretKey: CANYON_ACCESS_TOKEN
      remoteRef:
        key: ${release}
```

Add the kinds your template makes to the AppProjects, or Argo CD refuses to sync them.

## Related

- [Your platform runs the apps](/self-host/hand-off/): hand-off, the deploy repo and what Canyon commits.
- [Argo CD](/self-host/hand-off-argo-cd/) and [Flux](/self-host/hand-off-flux/): what syncs the deploy repo.
