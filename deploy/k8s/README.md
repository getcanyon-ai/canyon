# Canyon on your cluster

This directory is the install base: a kustomize base your overlay adds the values only you know to.

`CANYON_DATABASE_URL` and `CANYON_APP_DATABASE_URL`, in the Secret `canyon-server`, name their `sslmode`:
`verify-full` with `sslrootcert`, or `require` at the least. Canyon never falls back to plaintext: with no `sslmode`,
or `prefer`, it connects over TLS or not at all, and `allow` refuses to start. `disable`, as the bundled databases
in `canyon-infra` use, connects unencrypted, and the server's log says so as it starts.

Two Secrets in `canyon` are yours to write: `canyon-server`, the server's environment, and
`canyon-egress`, one key `token` that Canyon's egress gateway (`egress.yaml`) sends the server's
receiver on 7781 and the server checks, so both read it and neither starts without it. The server's
NetworkPolicy admits 7781 from the gateway's pods alone, and 7782, the policy the gateway's check pulls,
over TLS under a certificate the server mints at its first start. The install step
that publishes it, once the server is up, for the gateway to pin:

```sh
cert=$(kubectl -n canyon exec deploy/canyon-server -- /canyon-server egress-policy-cert) &&
  kubectl -n canyon create configmap canyon-egress-policy-cert --from-literal=tls.crt="$cert" --dry-run=client -o yaml | kubectl apply -f -
```

Running it again is safe. It lasts ten years. To renew it, delete its row on Canyon's Postgres (`DELETE FROM egress_policy_cert;`), restart the server, which mints another, run the step again, and restart the gateway (`kubectl -n canyon rollout restart deploy/canyon-egress`) so it pins the new one.

Each of the gateway's pods runs egress-control beside Envoy, the server's image as
`canyon-server egress-control`: Envoy asks it before every call, and answers `503` while it does not answer.
It pulls the policy with a token projected for the ServiceAccount `canyon-egress` (audience
`canyon-egress-policy`, ten minutes) and pins the certificate the step above published, so the gateway's pods
start only once that ConfigMap exists, and are ready only once egress-control holds a policy, which its exec
probe (`canyon-server egress-control probe`) tells. It keeps nothing: no volume holds a policy. It reads what
Envoy lost from Envoy's admin socket, on a memory volume of the pod's alone, and reports it to the server. A rollout
adds one pod before it ends one (`maxSurge: 1`, `maxUnavailable: 0`), and a pod stops answering at SIGTERM, so
three answer at once at most: `CANYON_EGRESS_REPLICAS`, which shares the rate limit, says 3. Never scale the
Deployment by hand or with an autoscaler: more replicas than it counts let more new names through a minute than
the limit says. Change `replicas` and `CANYON_EGRESS_REPLICAS` together, the latter one more than the former. Its Prometheus
metrics are on 7784 (`CANYON_EGRESS_CONTROL_METRICS_ADDR`; unset, it serves none), which the gateway's
NetworkPolicy admits from one namespace, `ingress[2]`: a placeholder no namespace is named until the overlay
patches in your scraper's, as it does the server's `egress[3..5]`.

The server checks the gateway's projected token against the cluster's service-account key set, which it
reads from the API server with its own token: the overlay patches the API server's endpoint addresses and
the `kubernetes` Service's cluster IP into the server's NetworkPolicy, `egress[5]`, as it does the databases'.
The policy carries the same key set to egress-control, which checks each app pod's token against it, offline
(design/EGRESS.md §4.3). An app env whose calls go through Canyon runs as its own ServiceAccount,
`canyon-app-<app>-<env>` in `canyon-apps`. The executor creates it and deletes it, and holds no other right
on ServiceAccounts: it can't read one, mint its token or bind it to a role.

Every NetworkPolicy here holds only where the cluster enforces it, so Canyon builds nothing where the front
door's check hasn't seen NetworkPolicy enforced: [Strict mode on the AWS VPC CNI](https://www.getcanyon.ai/docs/self-host/kubernetes/#strict-mode-on-the-aws-vpc-cni)
has the settings on EKS and how to check them.

The guides are on the docs site:

- [Install on Kubernetes](https://www.getcanyon.ai/docs/self-host/kubernetes/): what Canyon ships,
  what you bring, the overlay, the gateway and the IAM policy. Its
  [Host firewall](https://www.getcanyon.ai/docs/self-host/kubernetes/#host-firewall) section has the
  node rule the front-door check's warning points at.
- [One k3s host, step by step](https://www.getcanyon.ai/docs/self-host/k3s/)
- [Try it on Kubernetes (Helm)](https://www.getcanyon.ai/docs/self-host/helm/), the demo chart

## Run image

The ClusterStack `canyon`'s run image is `canyon-run` (`builds.yaml`), which your overlay names by digest.
To patch the operating-system packages under every app, move it to a newer digest and let the
ClusterBuilder `canyon` resolve it: Canyon rebases each app's image onto it by kpack's own rebase.
kpack copies the run image to
`<the ClusterBuilder's spec.tag>-run-image`, by default `<registry>/canyon-builder:clusterbuilder-canyon-run-image`,
and every app image records that name, so the ClusterStack's run image may move to any repository, a
mirror included. Keep the ClusterBuilder's name and `spec.tag`: changed, they name the run image
otherwise, Canyon never rebases an earlier image onto it, and each app moves at its next deploy.

No lifecycle rule on your registry's `canyon/*` repositories may expire tagged images: Canyon keeps
images for a rollback, a hold or a patch revert, and one
expired is answered `404` and rebuilt, not run as production ran it.
