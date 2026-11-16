#!/usr/bin/env python3
"""Checks `helm template` output of deploy/helm/canyon against deploy/k8s/deploy_test.go's invariants,
plus the chart's own: generated secrets agree across templates.

  helm template canyon deploy/helm/canyon -f values.yaml | deploy/helm/check-rendered.py

The egress gateway's token, canyon/canyon-egress's `token`: rendered from canyon.egress.token with
--egress-token <that value>, brought with canyon.egress.existingSecret with --existing-egress-secret, and
otherwise generated once and kept by lookup. Only a token from values carries checksum/egress-token.
"""
import argparse
import hashlib
import ipaddress
import os
import re
import sys
from urllib.parse import urlparse

import yaml

args = argparse.ArgumentParser()
args.add_argument("--egress-token", help="canyon.egress.token, which the rendered Secret must hold")
args.add_argument("--existing-egress-secret", action="store_true", help="canyon.egress.existingSecret: no Secret, no token sum")
args.add_argument("--in-cluster-signin", action="store_true", help="canyon.egress.signin.inCluster: the canyon-apps-signin policy is rendered")
args.add_argument("--ask-token", help="gateway.askToken, which both ask-token Secrets must hold, and whose sum rolls Caddy and the server")
args = args.parse_args()
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
fail = []


def find(kind, name, ns=None):
    for d in docs:
        if d["kind"] == kind and d["metadata"]["name"] == name and (ns is None or d["metadata"].get("namespace") == ns):
            return d
    fail.append(f"missing {kind} {ns}/{name}")
    return None


def images(o):
    if isinstance(o, dict):
        for k, v in o.items():
            if k == "image" and isinstance(v, str):
                yield v
            else:
                yield from images(v)
    elif isinstance(o, list):
        for v in o:
            yield from images(v)


# Every image by digest (TestEveryImageIsPinnedByDigest), the kpack builder objects included.
for d in docs:
    blobs = [d]
    if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "canyon-builder-objects":
        blobs += [x for x in yaml.safe_load_all(d["data"]["objects.yaml"]) if x]
    for b in blobs:
        for img in images(b):
            if not re.search(r"@sha256:[0-9a-f]{64}$", img):
                fail.append(f"{d['kind']} {d['metadata']['name']}: image not by digest: {img}")

# Every container of Canyon's namespaces bounded, read-only root (TestEveryContainerIsBounded).
for d in docs:
    if d["kind"] not in ("Deployment", "Job") or not d["metadata"].get("namespace", "").startswith("canyon"):
        continue
    for c in d["spec"]["template"]["spec"]["containers"]:
        r = c.get("resources", {})
        if d["metadata"]["namespace"] == "canyon":
            for part in ("requests", "limits"):
                for res in ("cpu", "memory"):
                    if not r.get(part, {}).get(res):
                        fail.append(f"{d['metadata']['name']}/{c['name']}: no {part}.{res}")
            if not c.get("securityContext", {}).get("readOnlyRootFilesystem"):
                fail.append(f"{d['metadata']['name']}/{c['name']}: root filesystem not read-only")

# The executor: one replica, Recreate, ingress from the server alone on 7778.
ex = find("Deployment", "canyon-executor", "canyon")
if ex and (ex["spec"]["replicas"] != 1 or ex["spec"]["strategy"]["type"] != "Recreate"):
    fail.append("executor must be 1 replica, Recreate")
exp = [d for d in docs if d["kind"] == "NetworkPolicy" and d["metadata"].get("namespace") == "canyon"
       and d["spec"]["podSelector"].get("matchLabels", {}).get("app.kubernetes.io/name") == "canyon-executor"
       and "Ingress" in d["spec"].get("policyTypes", [])]
if len(exp) != 1 or exp[0]["spec"]["ingress"] != [{"from": [{"podSelector": {"matchLabels": {"app.kubernetes.io/name": "canyon-server"}}}], "ports": [{"protocol": "TCP", "port": 7778}]}]:
    fail.append("executor ingress must be the server's pods on 7778 alone")
# Its egress keeps every rule of deploy/k8s/executor.yaml's policy (e.g. canyon-identity's /answered on 4183).
with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "k8s", "executor.yaml")) as f:
    base = next(d for d in yaml.safe_load_all(f) if d and d["kind"] == "NetworkPolicy")
for rule in base["spec"]["egress"]:
    if len(exp) == 1 and rule not in exp[0]["spec"].get("egress", []):
        fail.append(f"executor egress lacks deploy/k8s's {rule}")

# A floored app reaches Canyon's egress gateway and nothing else outside its pod (contracts/executor.md, On
# Kubernetes): policies add up, so every NetworkPolicy of canyon-apps but the executor's own per app leaves its
# pods, labelled canyon.floor, out — the k3s gateway's included: the chart opens no Caddy path for them (#2316) —
# but canyon-apps-signin, the in-cluster sign-in rule, held to its one shape below (#2326).
for d in docs:
    if d["kind"] != "NetworkPolicy" or d["metadata"].get("namespace") != "canyon-apps" or d["metadata"]["name"] == "canyon-apps-signin":
        continue
    if {"key": "canyon.floor", "operator": "DoesNotExist"} not in d["spec"]["podSelector"].get("matchExpressions", []):
        fail.append(f"NetworkPolicy canyon-apps/{d['metadata']['name']} selects floored pods (canyon.floor): its egress adds to their floor")

# CANYON_KUBE_SIGNIN_DIRECT (executor.md, Sign-in past the gateway): each entry <host> or <host>=<address>/32, the
# host a lower-case DNS name or an IPv4 address, the address one IPv4 address written with its /32, outside
# link-local. The executor refuses anything else at start; this says so before an install does.
HOST = re.compile(r"^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$")
direct = next((e.get("value", "") for e in ex["spec"]["template"]["spec"]["containers"][0].get("env", []) if e["name"] == "CANYON_KUBE_SIGNIN_DIRECT"), "") if ex else ""
hosts, inside = [], []  # inside: the hosts listed without an address, providers inside the cluster
for entry in direct.split(",") if direct else []:
    host, eq, addr = entry.strip().partition("=")
    if host in hosts:
        fail.append(f"CANYON_KUBE_SIGNIN_DIRECT names {host!r} twice: the executor refuses a host named twice")
    hosts.append(host)
    if not eq and host:
        inside.append(host)
    try:
        ipaddress.IPv4Address(host)
        named = True
    except ValueError:
        named = bool(HOST.match(host))
    try:
        net = ipaddress.IPv4Network(addr) if eq else None
    except ValueError:
        net = False
    if not named or net is False or (eq and ("/" not in addr or net.prefixlen != 32 or net.subnet_of(ipaddress.IPv4Network("169.254.0.0/16")))):
        fail.append(f"CANYON_KUBE_SIGNIN_DIRECT holds {entry!r}: each is <host> or <host>=<address>/32, one IPv4 address outside 169.254.0.0/16")

# Pod Security Admission (#2368): each namespace the chart writes enforces, warns and audits at one level, Canyon's
# four at deploy/k8s/namespaces.yaml's and the bundled parts' restricted (#2772).
LEVELS = {"canyon": "restricted", "canyon-apps": "baseline", "canyon-builds": "restricted", "canyon-scans": "restricted",
          "canyon-infra": "restricted", "canyon-auth": "restricted", "canyon-gateway": "restricted"}
for d in docs:
    if d["kind"] != "Namespace" or d["metadata"]["name"] == "kpack":  # kpack's own, from its release
        continue
    name, labels = d["metadata"]["name"], d["metadata"].get("labels") or {}
    got = {m: labels.get("pod-security.kubernetes.io/" + m) for m in ("enforce", "warn", "audit")}
    if name not in LEVELS:
        fail.append(f"Namespace {name} has no Pod Security level here: name one in LEVELS")
    elif got != dict.fromkeys(got, LEVELS[name]):
        fail.append(f"Namespace {name}'s Pod Security is {got}, want {LEVELS[name]} for each")

# Each pod the render makes passes its namespace's enforced level, as the Pod Security Standards define baseline and
# restricted (kubernetes.io/docs/concepts/security/pod-security-standards, every control of each, #2772): the API
# server would refuse one that does not, and a Deployment's refused pod is only a ReplicaSet event, never a failed
# apply. A pod in a namespace the render labels no level, or whose namespace it does not make, is checked at restricted.
BASELINE_CAPS = {"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD", "NET_BIND_SERVICE",
                 "SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT"}
SAFE_SYSCTLS = {"kernel.shm_rmid_forced", "net.ipv4.ip_local_port_range", "net.ipv4.ip_unprivileged_port_start",
                "net.ipv4.tcp_syncookies", "net.ipv4.ping_group_range", "net.ipv4.ip_local_reserved_ports",
                "net.ipv4.tcp_keepalive_time", "net.ipv4.tcp_fin_timeout", "net.ipv4.tcp_keepalive_intvl",
                "net.ipv4.tcp_keepalive_probes"}
SELINUX_TYPES = {None, "", "container_t", "container_init_t", "container_kvm_t", "container_engine_t"}
RESTRICTED_VOLUMES = {"configMap", "csi", "downwardAPI", "emptyDir", "ephemeral", "persistentVolumeClaim", "projected", "secret"}
ENFORCED = {d["metadata"]["name"]: (d["metadata"].get("labels") or {}).get("pod-security.kubernetes.io/enforce")
            for d in docs if d["kind"] == "Namespace"}


def pod_template(d):
    """The pod d makes, {metadata, spec}, or None for an object that makes none."""
    spec = d.get("spec") or {}
    if d["kind"] == "Pod":
        return d
    if d["kind"] in ("Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job"):
        return spec["template"]
    if d["kind"] == "CronJob":
        return spec["jobTemplate"]["spec"]["template"]
    return None


APPARMOR = "container.apparmor.security.beta.kubernetes.io/"


def pod_security(pod, level):
    """What of pod the level refuses, each in words; none when it passes."""
    spec, annotations = pod.get("spec") or {}, (pod.get("metadata") or {}).get("annotations") or {}
    out, psc = [], spec.get("securityContext") or {}
    for key, value in annotations.items():  # the beta annotation the API server still reads, beside appArmorProfile
        if key.startswith(APPARMOR) and value != "runtime/default" and not str(value).startswith("localhost/"):
            out.append(f"{key[len(APPARMOR):]}'s AppArmor annotation {value}")
    containers = [c for k in ("initContainers", "containers", "ephemeralContainers") for c in spec.get(k) or []]
    seccomp = lambda sc: (sc.get("seccompProfile") or {}).get("type")
    for host in ("hostNetwork", "hostPID", "hostIPC"):
        if spec.get(host):
            out.append(f"{host}")
    if any("hostPath" in v for v in spec.get("volumes") or []):
        out.append("a hostPath volume")
    if not {s["name"] for s in psc.get("sysctls") or []} <= SAFE_SYSCTLS:
        out.append("an unsafe sysctl")
    for sc in [psc] + [c.get("securityContext") or {} for c in containers]:
        se = sc.get("seLinuxOptions") or {}
        if se.get("type") not in SELINUX_TYPES or se.get("user") or se.get("role"):
            out.append("SELinux options baseline refuses")
        if seccomp(sc) == "Unconfined":
            out.append("seccomp Unconfined")
        if (sc.get("appArmorProfile") or {}).get("type") not in (None, "RuntimeDefault", "Localhost"):
            out.append("an AppArmor profile baseline refuses")
        if (sc.get("windowsOptions") or {}).get("hostProcess"):
            out.append("a Windows host process")
    for c in containers:
        sc, name = c.get("securityContext") or {}, c["name"]
        if sc.get("privileged"):
            out.append(f"{name} privileged")
        if not set((sc.get("capabilities") or {}).get("add") or []) <= BASELINE_CAPS:
            out.append(f"{name} adds a capability baseline refuses")
        if sc.get("procMount") not in (None, "Default"):
            out.append(f"{name}'s procMount")
        if any(p.get("hostPort") for p in c.get("ports") or []):
            out.append(f"{name} a hostPort")
    if level != "restricted":
        return out
    for v in spec.get("volumes") or []:
        if not set(v) - {"name"} <= RESTRICTED_VOLUMES:
            out.append(f"volume {v['name']} of a kind restricted refuses")
    if psc.get("runAsUser") == 0:
        out.append("the pod runs as user 0")
    for c in containers:
        sc, name = c.get("securityContext") or {}, c["name"]
        caps = sc.get("capabilities") or {}
        if sc.get("allowPrivilegeEscalation") is not False:
            out.append(f"{name} may escalate privileges")
        if not (sc.get("runAsNonRoot") is True or psc.get("runAsNonRoot") is True and sc.get("runAsNonRoot") is not False):
            out.append(f"{name} may run as root (runAsNonRoot)")
        if sc.get("runAsUser") == 0:
            out.append(f"{name} runs as user 0")
        if (seccomp(sc) or seccomp(psc)) not in ("RuntimeDefault", "Localhost"):
            out.append(f"{name} runs under no seccomp profile")
        if "ALL" not in (caps.get("drop") or []):
            out.append(f"{name} keeps capabilities (drop ALL)")
        if not set(caps.get("add") or []) <= {"NET_BIND_SERVICE"}:
            out.append(f"{name} adds a capability restricted refuses")
    return out


for d in docs:
    pod = pod_template(d)
    if pod is None:
        continue
    ns = d["metadata"].get("namespace")
    level = ENFORCED.get(ns) or "restricted"
    for why in pod_security(pod, level):
        fail.append(f"{d['kind']} {ns}/{d['metadata']['name']} breaks Pod Security {level}: {why}")

# The registry and Caddy start only once their user can write their volume (#2772, canyon.ownedOrExit): a volume a
# release before wrote as root would otherwise let them start half-broken, refusing pushes and storing no certificate.
for ns, name, volume in (("canyon-infra", "registry", "/var/lib/registry"), ("canyon-gateway", "caddy", "/data")):
    d = next((d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == name and d["metadata"].get("namespace") == ns), None)
    if d is None:
        continue  # the part is off
    spec = d["spec"]["template"]["spec"]
    init = next((c for c in spec.get("initContainers") or [] if c["name"] == "volume-owned"), {})
    cmd = init.get("command") or []
    if len(cmd) != 3 or cmd[:2] != ["sh", "-c"] or f"d={volume};" not in cmd[2] or "canyon: user" not in cmd[2] or \
            init.get("image") != spec["containers"][0]["image"] or {"name": "data", "mountPath": volume} not in (init.get("volumeMounts") or []):
        fail.append(f"Deployment {ns}/{name} starts without checking its user can write {volume} (canyon.ownedOrExit)")
    if spec["containers"][0].get("command") or spec["containers"][0].get("args"):  # the image's own start: an sh exec loses Caddy's capability
        fail.append(f"Deployment {ns}/{name}'s container replaces its image's own start")

# canyon-apps-signin (kubernetes.md, Install for production step 3; #2326): rendered from canyon.egress.signin.inCluster
# alone, it admits floored pods to one namespace's labelled pods on one TCP port and nothing else — never a namespace the
# chart writes or installs into, whose pods are Canyon's own, Caddy, Pocket ID, kpack or headlamp, nor kube-system's —
# and only beside a host CANYON_KUBE_SIGNIN_DIRECT lists without an address, the in-cluster entry its sidecars call
# directly. The namespaces are read from this render, so they can't drift from the template's own list: every Namespace
# object, each HelmChart's targetNamespace, kpack's even with its objects off, and kube-system.
OWN = {d["metadata"]["name"] for d in docs if d["kind"] == "Namespace"} | \
      {d["spec"]["targetNamespace"] for d in docs if d["kind"] == "HelmChart" and d["spec"].get("targetNamespace")} | {"kpack", "kube-system"}
signins = [d for d in docs if d["kind"] == "NetworkPolicy" and d["metadata"]["name"] == "canyon-apps-signin"]
if len(signins) != (1 if args.in_cluster_signin else 0):
    fail.append(f"{len(signins)} NetworkPolicy canyon-apps-signin rendered: one with canyon.egress.signin.inCluster, none without")
for d in signins:
    spec = d["spec"]
    rule = (spec.get("egress") or [{}])[0]
    peer = (rule.get("to") or [{}])[0]
    ns = peer.get("namespaceSelector", {}).get("matchLabels", {}).get("kubernetes.io/metadata.name")
    port = (rule.get("ports") or [{}])[0]
    shaped = (d["metadata"].get("namespace") == "canyon-apps" and set(spec) == {"podSelector", "policyTypes", "egress"}
              and spec["podSelector"] == {"matchLabels": {"canyon.floor": "true"}} and spec["policyTypes"] == ["Egress"]
              and len(spec["egress"]) == 1 and set(rule) == {"to", "ports"} and len(rule["to"]) == 1 and len(rule["ports"]) == 1
              and set(peer) == {"namespaceSelector", "podSelector"} and peer["namespaceSelector"] == {"matchLabels": {"kubernetes.io/metadata.name": ns}}
              and type(ns) is str and ns != "" and set(peer["podSelector"]) == {"matchLabels"} and peer["podSelector"]["matchLabels"]
              and all(type(k) is str and type(v) is str for k, v in peer["podSelector"]["matchLabels"].items())
              and set(port) == {"protocol", "port"} and port["protocol"] == "TCP" and type(port["port"]) is int and 0 < port["port"] < 65536)
    if not shaped:
        fail.append("NetworkPolicy canyon-apps/canyon-apps-signin is not floored pods to one namespace's labelled pods on one TCP port")
    elif ns in OWN:
        fail.append(f"NetworkPolicy canyon-apps/canyon-apps-signin admits floored pods to {ns}, a namespace of Canyon's or the cluster's own: never a gateway's or Canyon's pods")
    if not inside:
        fail.append("NetworkPolicy canyon-apps/canyon-apps-signin without a host CANYON_KUBE_SIGNIN_DIRECT lists in-cluster, with no address")

# On k3s, the bundled Pocket ID behind the chart's Caddy is out of a floored app's reach: the server learns it from
# CANYON_KUBE_GATEWAY_SIGNIN, Pocket ID's issuer, and refuses via canyon while the front door signs in there (#2316).
# It is no secret, so it is the server container's own env, there with canyon.existingSecret too.
server_env = {e["name"]: e.get("value") for e in (find("Deployment", "canyon-server", "canyon") or {"spec": {"template": {"spec": {"containers": [{}]}}}})["spec"]["template"]["spec"]["containers"][0].get("env", [])}
if server_env.get("CANYON_AUTH") != "oidc":
    fail.append("the server's env must pin CANYON_AUTH to oidc: env wins over envFrom, so no Secret, existingSecret included, starts it without sign-in")
k3s_caddy = any(d["kind"] == "NetworkPolicy" and d["metadata"]["name"] == "canyon-apps-gateway" for d in docs)
pocket = any(d["kind"] == "Deployment" and d["metadata"]["name"] == "pocket-id" for d in docs)
if (k3s_caddy and pocket) != ("CANYON_KUBE_GATEWAY_SIGNIN" in server_env):
    fail.append("the server's CANYON_KUBE_GATEWAY_SIGNIN must be set exactly when the chart serves Pocket ID behind its k3s gateway")
elif "CANYON_KUBE_GATEWAY_SIGNIN" in server_env and not str(server_env["CANYON_KUBE_GATEWAY_SIGNIN"]).startswith("https://"):
    fail.append("the server's CANYON_KUBE_GATEWAY_SIGNIN is Pocket ID's issuer, an https URL")

# Every build pod's way out (contracts/executor.md, A build reaches no private or link-local address;
# TestTheBuildsReachNoPrivateAddress): NetworkPolicies add up, so every one of canyon-builds, together, lets a build
# pod out to kube-dns on 53, to TCP 80 and 443 at every address outside the base's builds policy's list, which
# executor/kube's TestTheBuildsRefuseTheOneList holds to objects.go's private, and on k3s to the bundled registry's
# pods on 5000; one port each, nowhere else, and no peer to the metadata address or the WireServer.
with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "k8s", "builds.yaml")) as f:
    FLOOR = next(to["ipBlock"]["except"] for d in yaml.safe_load_all(f) if d and d["kind"] == "NetworkPolicy"
                 for rule in d["spec"]["egress"] for to in rule["to"] if "ipBlock" in to)
builds = [d for d in docs if d["kind"] == "NetworkPolicy" and d["metadata"].get("namespace") == "canyon-builds"]
if not any(b["spec"].get("podSelector") == {} and "Egress" in b["spec"].get("policyTypes", []) for b in builds):
    fail.append("no NetworkPolicy holds every pod of canyon-builds")
for b in builds:
    name = f"NetworkPolicy canyon-builds/{b['metadata']['name']}"
    for rule in b["spec"].get("egress") or []:
        if not rule.get("to") or not rule.get("ports") or any("port" not in p or "endPort" in p for p in rule["ports"]):
            fail.append(f"{name} lets a build pod out to every address, or on every port or a range of them: {rule}")
            continue
        ports = sorted(p["port"] for p in rule["ports"])
        for to in rule["to"]:
            block = to.get("ipBlock")
            if block is not None:
                net = ipaddress.ip_network(block["cidr"])
                except_ = [ipaddress.ip_network(x) for x in block.get("except") or []]
                for meta in ("169.254.169.254", "168.63.129.16"):
                    if ipaddress.ip_address(meta) in net and not any(ipaddress.ip_address(meta) in x for x in except_):
                        fail.append(f"{name} lets a build pod reach {meta}")
                if block["cidr"] != "0.0.0.0/0" or any(f not in (block.get("except") or []) for f in FLOOR) or ports != [80, 443]:
                    fail.append(f"{name} lets a build pod out to {block} on {ports}: TCP 80 and 443 outside {FLOOR} alone")
            elif to.get("namespaceSelector") == {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}} and to.get("podSelector") == {"matchLabels": {"k8s-app": "kube-dns"}}:
                if set(ports) != {53}:
                    fail.append(f"{name} lets a build pod reach kube-dns on {ports}: 53 alone")
            elif to.get("namespaceSelector") == {"matchLabels": {"kubernetes.io/metadata.name": "canyon-infra"}} and to.get("podSelector") == {"matchLabels": {"app": "registry"}}:
                if ports != [5000]:
                    fail.append(f"{name} lets a build pod reach the bundled registry on {ports}: 5000 alone")
            else:
                fail.append(f"{name} lets a build pod out to {to}")

# The server's policy: 6 egress rules, the databases and the API server last (TestTheServerHasANetworkPolicy).
sp = find("NetworkPolicy", "canyon-server", "canyon")
if sp:
    eg = sp["spec"]["egress"]
    private = ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"]
    if len(eg) != 6:
        fail.append(f"server egress has {len(eg)} rules, not 6")
    else:
        if eg[1]["to"] != [{"podSelector": {"matchLabels": {"app.kubernetes.io/name": "canyon-executor"}}}]:
            fail.append("server egress[1] is not the executor")
        if eg[2]["to"][0]["ipBlock"] != {"cidr": "0.0.0.0/0", "except": private} or eg[2]["ports"] != [{"protocol": "TCP", "port": 443}]:
            fail.append("server egress[2] is not public 443")
        # egress[5], the API server, holds its endpoints and the kubernetes Service's cluster IP (#2383)
        for i, ports in ((3, [5432]), (4, [5433]), (5, [443, 6443])):
            to = eg[i]["to"]
            if not to or (i < 5 and len(to) != 1) or any(not t.get("ipBlock", {}).get("cidr") for t in to) or eg[i]["ports"] != [{"protocol": "TCP", "port": p} for p in ports]:
                fail.append(f"server egress[{i}] is not {'one ipBlock' if i < 5 else 'ipBlocks, each with a cidr,'} on {ports}")

# The router rolls when its config changes (TestTheRouterRollsWhenItsConfigChanges).
rc, rd = find("ConfigMap", "canyon-router", "canyon"), find("Deployment", "canyon-router", "canyon")
if rc and rd and rd["spec"]["template"]["metadata"].get("annotations", {}).get("canyon.nginx-conf-sha256") != \
        hashlib.sha256(rc["data"]["nginx.conf"].encode()).hexdigest():
    fail.append("the router's canyon.nginx-conf-sha256 is not nginx.conf's sha256")

# Apps build on canyon-build and run on canyon-run, read with canyon-build's credential
# (TestTheStackRunsAppsOnCanyonRun).
bo = find("ConfigMap", "canyon-builder-objects", "canyon")
if bo:
    stack = next((x for x in yaml.safe_load_all(bo["data"]["objects.yaml"]) if x and x["kind"] == "ClusterStack"), None)
    if not stack or "canyon-run@" not in stack["spec"]["runImage"]["image"] or \
            "canyon-build@" not in stack["spec"]["buildImage"]["image"] or \
            stack["spec"].get("serviceAccountRef") != {"name": "canyon-build", "namespace": "canyon-builds"}:
        fail.append("the ClusterStack must build apps on canyon-build and run them on canyon-run, read with canyon-builds/canyon-build")

# The RBAC contract: no wildcards, Secrets only created/deleted, no Role in canyon.
for d in docs:
    if d["kind"] in ("Role", "ClusterRole") and d["metadata"]["name"].startswith("canyon"):
        if d["kind"] == "Role" and d["metadata"]["namespace"] == "canyon":
            fail.append(f"Role in canyon: {d['metadata']['name']}")
        for r in d.get("rules", []):
            if "*" in r.get("verbs", []) + r.get("resources", []) + r.get("apiGroups", []):
                fail.append(f"{d['kind']} {d['metadata']['name']}: wildcard")
            if "secrets" in r.get("resources", []) and set(r["verbs"]) - {"create", "delete"}:
                fail.append(f"{d['kind']} {d['metadata']['name']}: secrets beyond create/delete")

# Generated secrets agree across templates.
def secret(ns, name):  # optional: the bundled parts may be off
    s = next((d for d in docs if d["kind"] == "Secret" and d["metadata"]["name"] == name
              and d["metadata"].get("namespace") == ns), None)
    return (s or {}).get("stringData", {})


srv = secret("canyon", "canyon-server")
if srv:
    pg = secret("canyon-infra", "canyon-postgres").get("password")
    if pg and urlparse(srv["CANYON_DATABASE_URL"]).password != pg:
        fail.append("CANYON_DATABASE_URL password differs from Secret canyon-postgres")
    apg = secret("canyon-infra", "app-postgres").get("password")
    if apg and urlparse(srv.get("CANYON_APP_DATABASE_URL", "")).password != apg:
        fail.append("CANYON_APP_DATABASE_URL password differs from Secret app-postgres")
    cl = secret("canyon-auth", "pocket-id-clients")
    if cl and srv.get("CANYON_OIDC_CLIENT_SECRET") != cl.get("canyon"):
        fail.append("CANYON_OIDC_CLIENT_SECRET differs from pocket-id-clients/canyon")
    bs = secret("canyon-auth", "canyon-bootstrap")
    if bs and (bs["CLIENT_SECRET_CANYON"] != cl.get("canyon") or bs["CLIENT_SECRET_CANYON_APPS"] != cl.get("canyonApps")):
        fail.append("bootstrap client secrets differ from pocket-id-clients")

# Canyon's egress gateway is deploy/k8s/egress.yaml's, object for object: Envoy's bootstrap byte for byte, which
# deploy_test.go holds to deploy/egress, its Deployment, PDB, Service and NetworkPolicy; and the server takes its
# records as deploy/k8s/server.yaml's does — the receiver's env, port, Service port and ingress from the gateway
# alone (design/EGRESS.md §2.2, #2286). Without them every /run with egress on a Helm host answers 502. Its
# egress-control runs the server's image, each install's own (design/EGRESS.md §3.1).
k8s = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "k8s")


def server_image(objs):
    for d in objs:
        if d["kind"] == "Deployment" and d["metadata"]["name"] == "canyon-server":
            return d["spec"]["template"]["spec"]["containers"][0]["image"]


with open(os.path.join(k8s, "server.yaml")) as f:
    base_server = server_image(d for d in yaml.safe_load_all(f) if d)
with open(os.path.join(k8s, "egress.yaml")) as f:
    for b in (d for d in yaml.safe_load_all(f) if d):
        d = find(b["kind"], b["metadata"]["name"], b["metadata"]["namespace"])
        part = "data" if b["kind"] == "ConfigMap" else "spec"
        got = yaml.safe_load(yaml.safe_dump(d.get(part))) if d else None  # a copy, its token's sum taken out
        if got and b["kind"] == "NetworkPolicy":  # the scraper's namespace, canyon.egress.metrics.namespace, the base's placeholder
            sel = got["ingress"][2]["from"][0]["namespaceSelector"]["matchLabels"]
            if not sel.get("kubernetes.io/metadata.name"):
                fail.append("NetworkPolicy canyon/canyon-egress admits egress-control's metrics from no namespace")
            sel["kubernetes.io/metadata.name"] = b[part]["ingress"][2]["from"][0]["namespaceSelector"]["matchLabels"]["kubernetes.io/metadata.name"]
        if got and b["kind"] == "Deployment":
            got["template"]["metadata"].get("annotations", {}).pop("checksum/egress-token", None)
            for spec, image in ((got, server_image(docs)), (b[part], base_server)):
                for c in spec["template"]["spec"]["containers"]:
                    if c["name"] == "egress-control" and c.get("image") == image:
                        c["image"] = "the server's"
        if d and got != b[part]:
            fail.append(f"{b['kind']} canyon/{b['metadata']['name']}'s {part} is not deploy/k8s/egress.yaml's")
# The gateway's ServiceAccount, whose projected token the server's policy listener takes (deploy/k8s/rbac.yaml):
# there, mounting no token of the API's own, and named by no binding.
sa = find("ServiceAccount", "canyon-egress", "canyon")
if sa and sa.get("automountServiceAccountToken") is not False:
    fail.append("ServiceAccount canyon/canyon-egress mounts the API's token: want automountServiceAccountToken false")
for d in docs:
    if d["kind"] in ("RoleBinding", "ClusterRoleBinding") and any(x.get("name") == "canyon-egress" for x in d.get("subjects") or []):
        fail.append(f"{d['kind']} {d['metadata']['name']} binds canyon-egress: it holds no Role")
# The token: the value given, else generated; never a Secret beside one the operator brought. A token from
# values puts its sum on each pod that reads it, so a new one rolls both Envoy and the server; a generated one
# never changes and carries none — under helm template each render would make a new one and roll both at every
# GitOps sync — nor does a brought one. The chart's Secret is kept on uninstall and on a switch to a brought one.
rendered = next((d for d in docs if d["kind"] == "Secret" and d["metadata"]["name"] == "canyon-egress" and d["metadata"].get("namespace") == "canyon"), None)
token = (rendered or {}).get("stringData", {}).get("token", "")
sums = {}
for name in ("canyon-egress", "canyon-server"):
    d = next((d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == name and d["metadata"].get("namespace") == "canyon"), None)
    sums[name] = ((d or {}).get("spec", {}).get("template", {}).get("metadata", {}).get("annotations") or {}).get("checksum/egress-token")
if args.existing_egress_secret:
    if rendered or any(sums.values()):
        fail.append("with canyon.egress.existingSecret the chart renders no Secret canyon/canyon-egress and no checksum/egress-token")
else:
    if not re.fullmatch(r"[A-Za-z0-9._~+/=-]{32,512}", token):
        fail.append("Secret canyon/canyon-egress has no token the server and Envoy take (32 to 512 of [A-Za-z0-9._~+/=-])")
    if rendered and (rendered["metadata"].get("annotations") or {}).get("helm.sh/resource-policy") != "keep":
        fail.append("Secret canyon/canyon-egress is not helm.sh/resource-policy: keep: a switch to existingSecret would delete it mid-upgrade")
    if args.egress_token is not None and token != args.egress_token:
        fail.append("Secret canyon/canyon-egress's token is not canyon.egress.token")
    want = hashlib.sha256(token.encode()).hexdigest()[:16] if args.egress_token is not None else None
    for name, got in sums.items():
        if got != want:
            fail.append(f"Deployment canyon/{name}'s checksum/egress-token is {got!r}, want {want!r}: a token from values rolls it, a generated one never")
with open(os.path.join(k8s, "server.yaml")) as f:
    base = {d["kind"]: d for d in yaml.safe_load_all(f) if d}
bc, sd, ss = base["Deployment"]["spec"]["template"]["spec"]["containers"][0], find("Deployment", "canyon-server", "canyon"), find("Service", "canyon-server", "canyon")
if sd:
    c = sd["spec"]["template"]["spec"]["containers"][0]
    for e in (e for e in bc["env"] if e["name"].startswith("CANYON_EGRESS_")):
        if e not in c.get("env", []):
            fail.append(f"the server's env lacks deploy/k8s's {e}")
    for port in bc["ports"]:
        if port not in c.get("ports", []):
            fail.append(f"the server's container lacks deploy/k8s's port {port}")
if ss:
    for port in base["Service"]["spec"]["ports"]:
        if port not in ss["spec"]["ports"]:
            fail.append(f"the server's Service lacks deploy/k8s's port {port}")
if sp and sp["spec"].get("ingress") != base["NetworkPolicy"]["spec"]["ingress"]:
    fail.append("the server's ingress is not deploy/k8s's: 7777 from anywhere, the receiver from the gateway's pods alone")

# The bundled gateway asks Canyon's /tls/ask with the token the chart generates (#2672): Caddy is a pod with
# no address the server could trust on every network. The Caddyfile names the token as {env.…}, Caddy and the
# server read it from their own namespace's Secret, the two hold the same value, a value the server accepts,
# and no other object carries it as a literal.
caddy_cm = next((d for d in docs if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "caddy"), None)
if caddy_cm:
    caddyfile = caddy_cm["data"]["Caddyfile"]
    if "ask http://canyon-server.canyon.svc.cluster.local:7777/tls/ask?token={env.CANYON_TLS_ASK_TOKEN}" not in caddyfile:
        fail.append("the Caddyfile's on-demand ask is not Canyon's /tls/ask with {env.CANYON_TLS_ASK_TOKEN}")
    if "localhost:5555" in caddyfile:
        fail.append("the Caddyfile still answers its own ask, which admits any app name")
    held = [find("Secret", name, ns) for name, ns in (("canyon-tls-ask", "canyon"), ("caddy-tls-ask", "canyon-gateway"))]
    tokens = [s.get("stringData", {}).get("token", "") for s in held if s]
    if len(tokens) == 2 and tokens[0] != tokens[1]:
        fail.append("canyon/canyon-tls-ask and canyon-gateway/caddy-tls-ask hold different tokens")
    if tokens and not re.fullmatch(r"[A-Za-z0-9._~-]{32,512}", tokens[0]):
        fail.append("the ask token is not one the server accepts: 32 to 512 of [A-Za-z0-9._~-]")
    for dep, ns, secret in (("caddy", "canyon-gateway", "caddy-tls-ask"), ("canyon-server", "canyon", "canyon-tls-ask")):
        d = find("Deployment", dep, ns)
        env = d and next((e for e in d["spec"]["template"]["spec"]["containers"][0].get("env", []) if e["name"] == "CANYON_TLS_ASK_TOKEN"), None)
        if d and env != {"name": "CANYON_TLS_ASK_TOKEN", "valueFrom": {"secretKeyRef": {"name": secret, "key": "token"}}}:
            fail.append(f"{ns}/{dep} does not read CANYON_TLS_ASK_TOKEN from Secret {secret}'s token, and only from there: {env}")
    for d in docs:
        if tokens and tokens[0] and d["kind"] != "Secret" and tokens[0] in yaml.safe_dump(d):
            fail.append(f"{d['kind']} {d['metadata'].get('namespace')}/{d['metadata']['name']} carries the ask token as a literal")
    # A token from values (gateway.askToken, for helm template, where lookup sees nothing) is the one both Secrets
    # hold, and its sum rolls Caddy and the server together; a generated one never changes and carries no sum.
    if args.ask_token and tokens and tokens[0] != args.ask_token:
        fail.append("gateway.askToken is set, but the ask-token Secrets hold another token")
    want = hashlib.sha256(args.ask_token.encode()).hexdigest()[:16] if args.ask_token else None
    for dep, ns in (("caddy", "canyon-gateway"), ("canyon-server", "canyon")):
        d = find("Deployment", dep, ns)
        got = d and (d["spec"]["template"]["metadata"].get("annotations") or {}).get("checksum/tls-ask-token")
        if d and got != want:
            fail.append(f"{ns}/{dep}'s checksum/tls-ask-token is {got!r}, want {want!r}: a token from values rolls it, a generated one never")
elif args.ask_token:
    fail.append("--ask-token, but the chart rendered no gateway")
else:  # gateway.enabled=false: nothing asks, so the server is handed no token and no Secret holds one
    sd_ = next((d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == "canyon-server"), None)
    if sd_ and any(e["name"] == "CANYON_TLS_ASK_TOKEN" for e in sd_["spec"]["template"]["spec"]["containers"][0].get("env", [])):
        fail.append("with no gateway the server still reads CANYON_TLS_ASK_TOKEN")
    for d in docs:
        if d["kind"] == "Secret" and d["metadata"]["name"] in ("canyon-tls-ask", "caddy-tls-ask"):
            fail.append(f"with no gateway the chart still renders Secret {d['metadata']['name']}")

print("\n".join(fail) or f"ok: {len(docs)} objects")
sys.exit(1 if fail else 0)
