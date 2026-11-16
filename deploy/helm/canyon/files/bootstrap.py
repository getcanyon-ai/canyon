"""Canyon's post-install/upgrade bootstrap (templates/hooks/bootstrap.yaml). Standard library only.

1. Pocket ID: upsert the group, the users and the clients from values, through its API with the static
   admin key. A client's secret is added only when no active secret carries its prefix, and older
   secrets are then removed, so a rerun changes nothing.
2. Canyon: once /ready, sign in as the first user (a one-time Pocket ID token, then Canyon's own OIDC
   flow). PUT the preferences from values, finishing setup if bootstrap.completeSetup says so, then the
   front door when it differs from values, or when the stored one is not seen enforcing NetworkPolicy, so
   a plain helm upgrade saves it again (Canyon refuses it while the host is in setup). Canyon runs its
   probe and writes the audit row; a refused probe fails the Job with Canyon's message.

Configuration is /config/config.json; secrets come from the environment, never printed.
"""
import http.cookiejar
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

CFG = json.load(open(os.environ.get("CANYON_BOOTSTRAP_CONFIG", "/config/config.json")))  # the hook's ConfigMap; a test names its own
PID = CFG["pocketId"]["url"]
CANYON = CFG["canyon"]["url"]
KEY = os.environ["STATIC_API_KEY"]


def log(*a):
    print(*a, flush=True)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


JAR = http.cookiejar.CookieJar()
BROWSER = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(JAR), NoRedirect)
PLAIN = urllib.request.build_opener(NoRedirect)


def call(opener, method, url, body=None, headers=None):
    """(status, headers, parsed body or text); 3xx and 4xx/5xx are answers, not exceptions."""
    data = None if body is None else json.dumps(body).encode()
    h = {"Accept": "application/json", **({"Content-Type": "application/json"} if data else {}), **(headers or {})}
    req = urllib.request.Request(url, data=data, method=method, headers=h)
    try:
        r = opener.open(req, timeout=60)
        status, rh, raw = r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        status, rh, raw = e.code, e.headers, e.read()
    try:
        parsed = json.loads(raw) if raw else None
    except ValueError:
        parsed = raw.decode(errors="replace")
    return status, rh, parsed


def api(method, path, body=None, ok=(200, 201, 204)):
    status, _, out = call(PLAIN, method, f"{PID}/api/{path}", body, {"X-API-KEY": KEY})
    if status not in ok:
        sys.exit(f"pocket-id {method} /api/{path}: {status} {str(out)[:300]}")
    return out


def items(out):
    return out["data"] if isinstance(out, dict) and "data" in out else (out or [])


def wait(url, what, timeout=900):
    deadline = time.time() + timeout
    while True:
        try:
            if 200 <= call(PLAIN, "GET", url)[0] < 300:  # Pocket ID's /healthz answers 204
                return
        except OSError:
            pass
        if time.time() > deadline:
            sys.exit(f"{what} did not answer 2xx at {url} within {timeout}s")
        time.sleep(5)


def setup_pocket_id():
    c = CFG["pocketId"]
    groups = {g["name"]: g for g in items(api("GET", "user-groups?pagination[limit]=100"))}
    gid = groups[c["group"]]["id"] if c["group"] in groups else \
        api("POST", "user-groups", {"name": c["group"], "friendlyName": c["group"]})["id"]

    existing = {u["username"]: u for u in items(api("GET", "users?pagination[limit]=100"))}
    ids = []
    for u in c["users"]:
        body = {"username": u["username"], "email": u["email"], "firstName": u.get("firstName", ""),
                "lastName": u.get("lastName", ""), "displayName": " ".join(
                    x for x in (u.get("firstName"), u.get("lastName")) if x) or u["username"],
                "emailVerified": True, "isAdmin": True}
        cur = existing.get(u["username"])
        if cur is None:
            cur = api("POST", "users", body)
            log(f"pocket-id: created user {u['username']}")
        ids.append(cur["id"])
    members = {m["id"] for m in (api("GET", f"user-groups/{gid}").get("users") or [])}
    if not set(ids) <= members:
        api("PUT", f"user-groups/{gid}/users", {"userIds": sorted(members | set(ids))})
        log("pocket-id: group members updated")

    clients = {x["id"]: x for x in items(api("GET", "oidc/clients?pagination[limit]=100"))}
    for cl in c["clients"]:
        body = {"name": cl["name"], "callbackURLs": cl["callbacks"], "logoutCallbackURLs": [],
                "isPublic": False, "pkceEnabled": True, "requiresReauthentication": False,
                "skipConsent": True, "isGroupRestricted": True}
        if cl["id"] in clients:
            api("PUT", f"oidc/clients/{cl['id']}", body)
        else:
            api("POST", "oidc/clients", {**body, "id": cl["id"]})
            log(f"pocket-id: created client {cl['id']}")
        api("PUT", f"oidc/clients/{cl['id']}/allowed-user-groups", {"userGroupIds": [gid]})
        secret = os.environ[cl["secretEnv"]]
        stored = items(api("GET", f"oidc/clients/{cl['id']}/secrets"))
        if not any(s.get("prefix") and s.get("isActive", True) and secret.startswith(s["prefix"]) for s in stored):
            api("POST", f"oidc/clients/{cl['id']}/secrets", {"secret": secret})
            for s in stored:
                api("DELETE", f"oidc/clients/{cl['id']}/secrets/{s['id']}")
            log(f"pocket-id: client {cl['id']} secret set")
    log("pocket-id: configured")


def follow(url, limit=10):
    """GET through redirects with the browser jar; returns the last (status, headers, body, url)."""
    for _ in range(limit):
        status, h, body = call(BROWSER, "GET", url, headers={"Accept": "text/html,application/json"})
        if status not in (301, 302, 303, 307, 308):
            return status, h, body, url
        url = urllib.parse.urljoin(url, h["Location"])
        if url.startswith(PID + "/interaction"):
            url = complete_interaction(urllib.parse.parse_qs(urllib.parse.urlparse(url).query)["interaction"][0])
    sys.exit(f"too many redirects ending at {url}")


def complete_interaction(iid):
    for _ in range(5):
        status, _, s = call(BROWSER, "GET", f"{PID}/api/oidc/interactions/{iid}")
        if status != 200:
            sys.exit(f"pocket-id interaction {iid}: {status} {s}")
        step = s.get("currentStep") or "consent"
        status, _, r = call(BROWSER, "POST", f"{PID}/api/oidc/interactions/{iid}/complete", {"step": step})
        if status != 200:
            sys.exit(f"pocket-id interaction step {step}: {status} {r}")
        if r.get("redirectUrl"):
            return urllib.parse.urljoin(PID, r["redirectUrl"])
    sys.exit("pocket-id interaction did not finish")


def canyon_session():
    user = CFG["pocketId"]["users"][0]["username"]
    uid = next(u["id"] for u in items(api("GET", "users?pagination[limit]=100")) if u["username"] == user)
    token = api("POST", f"users/{uid}/one-time-access-token", {"ttl": "15m"})["token"]
    status, _, out = call(BROWSER, "POST", f"{PID}/api/one-time-access-token/{token}")
    if status != 200:
        sys.exit(f"pocket-id sign-in as {user}: {status} {out}")
    status, _, _, url = follow(f"{CANYON}/auth/start")
    if not url.startswith(CANYON + "/auth/callback") and status != 200:
        sys.exit(f"canyon sign-in ended at {url} with {status}")
    if not any(c.domain.lstrip(".") == urllib.parse.urlparse(CANYON).hostname for c in JAR):
        sys.exit("canyon sign-in set no session cookie (is the user on CANYON_ADMINS and an allowed email?)")
    log(f"canyon: signed in as {user}")


def canyon(method, path, body=None):
    return call(BROWSER, method, f"{CANYON}/api/v1/{path}", body, {"Origin": CANYON})


def comparable(fd):
    if not fd:
        return None
    keys = ("choice", "gateway", "issuer", "audience", "jwks_url", "cookie", "alb_arn", "domain", "form")
    out = {k: fd[k] for k in keys if fd.get(k) not in (None, "")}
    if fd.get("signin"):
        out["signin"] = {"issuer": fd["signin"]["issuer"], "client_id": fd["signin"]["client_id"]}
    return out


def set_front_door():
    want = CFG["frontDoor"]
    status, _, cur = canyon("GET", "front-door")
    if status != 200:
        sys.exit(f"canyon GET /front-door: {status} {cur}")
    stored = cur.get("front_door")
    same = comparable(stored) == comparable(want)
    # nothing builds behind a door no save found enforcing NetworkPolicy, so an unchanged one is saved again (#2514)
    heal = same and not stored.get("enforcing")
    if same and not heal and not CFG.get("forceFrontDoor"):
        log("canyon: front door unchanged")
        return
    if heal:
        log("canyon: front door not seen enforcing NetworkPolicy, so saving it again for Canyon to check the cluster")
    body = json.loads(json.dumps(want))
    if body.get("signin"):
        body["signin"]["client_secret"] = os.environ["FRONT_DOOR_CLIENT_SECRET"]
    status, _, out = canyon("PUT", "front-door", body)
    refused = out if isinstance(out, dict) else {}
    if heal and ((status == 409 and refused.get("error") == "probe_failed") or status >= 500):
        # Not saved again: the probe refused it, or couldn't run (the hook runs post-upgrade, while the executor may
        # still be rolling out). The door stays as it was, not enforcing, so nothing builds behind it (fail closed),
        # and the upgrade completes. Any other answer, a changed door or a forced save refused fails the Job, below.
        log(f"canyon: front door not saved again, so still not enforcing: {status} {refused.get('error', '')}: {refused.get('message', out)}")
        _, _, now = canyon("GET", "front-door")
        if isinstance(now, dict) and now.get("probe_note"):
            log(f"canyon: {now['probe_note']}")
        return
    if status != 200:
        sys.exit(f"canyon PUT /front-door: {status} {out}")
    log(f"canyon: front door set: choice {out['front_door']['choice']}, {out['front_door']['domain']}, {out['front_door']['form']}")


def set_preferences():
    """The preferences from values; on a host still in setup, also onboarded_at when completeSetup is on
    (the dashboard's Finish). Returns whether the host is set up."""
    prefs = dict(CFG.get("preferences") or {})
    status, _, cur = canyon("GET", "preferences")
    if status != 200:
        sys.exit(f"canyon GET /preferences: {status} {cur}")
    in_setup = cur.get("onboarded_at") is None
    if in_setup and CFG.get("completeSetup"):
        prefs["onboarded_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    if prefs:
        status, _, out = canyon("PUT", "preferences", prefs)
        if status != 200:
            sys.exit(f"canyon PUT /preferences: {status} {out}")
        log(f"canyon: preferences set: {', '.join(sorted(prefs))}")
        if "onboarded_at" in prefs:
            log("canyon: setup completed")
            return True
    if in_setup:
        log("canyon: still in setup, so the front door waits: finish setup in the dashboard "
            "(or set bootstrap.completeSetup), then run helm upgrade")
    return not in_setup


def login_link():
    user = CFG["pocketId"]["users"][0]["username"]
    uid = next(u["id"] for u in items(api("GET", "users?pagination[limit]=100")) if u["username"] == user)
    token = api("POST", f"users/{uid}/one-time-access-token", {"ttl": "1h"})["token"]
    log(f"pocket-id: one-time sign-in link for {user} (1 h, register a passkey): {PID}/lc/{token}")


if __name__ == "__main__":
    wait(f"{PID}/healthz", "pocket-id")
    setup_pocket_id()
    if CFG.get("frontDoor") or CFG.get("preferences") or CFG.get("completeSetup"):
        wait(f"{CANYON}/ready", "canyon")
        canyon_session()
        if set_preferences() and CFG.get("frontDoor"):
            set_front_door()
    if CFG.get("printLoginLink"):
        login_link()
