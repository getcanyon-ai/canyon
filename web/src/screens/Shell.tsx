import { useEffect, useRef, useState } from "react";
import { getAuthConfig, getFleet, getPreferences, UNAUTHENTICATED, type Preferences, type Viewer } from "../api";
import { S } from "../strings";
import { RailContext } from "../ui/Rail";
import { App } from "./App";
import { Approve } from "./Approve";
import { AuditTrail } from "./AuditTrail";
import { Device } from "./Device";
import { Fleet } from "./Fleet";
import { Guardrails } from "./Guardrails";
import { NoAccess } from "./NoAccess";
import { Onboarding } from "./Onboarding";
import { hashQuery } from "./query";
import { Settings } from "./Settings";
import { SetupNotice } from "./SetupNotice";
import { SignIn, takeReturn } from "./SignIn";

// The dashboard, and the one question asked before any of it is drawn: is this a host nobody has
// configured? Both halves are needed. `auth` alone says nothing about whether somebody has finished
// the form, and a null `onboarded_at` alone is also every demo host, where authentication is off
// and the marker is never set — keying on it would put the onboarding screen in front of every
// `docker compose up` (design/ONBOARDING.md §2). A read that fails draws the dashboard: the server
// refuses every /api/v1 route with 409 while the host is in setup, so the screens say so themselves
// rather than this gate guessing. The same read names the org, which the fleet's heading is.
// `answered` is told when the setup gate's read is answered: this tab had a session, so a later 401 is
// one that ended (design/specs/screens.md, A0).
function Dashboard({ hash, answered }: { hash: string; answered: () => void }) {
  const [setup, setSetup] = useState<Preferences | null | undefined>(undefined);
  const [org, setOrg] = useState<string | null>(null);
  // The same /auth/config read the setup gate makes below, kept for the app page's Access card: a
  // host with no sign-in routes none of that group (contracts/routes.md, Access), so the card must
  // know before it asks, not from the 404 it would otherwise log. Starts unknown, not "oidc": the
  // setup gate below awaits this same read before drawing the app page at all, but nothing here
  // should have to trust that ordering to stay a card that never asks too early.
  const [auth, setAuth] = useState<"oidc" | "off" | null>(null);
  // Who is reading, for the foot of the rail on every page. Only a host with sign-in has somebody to
  // name; the fleet read is where the contract answers it (routes.md, `viewer` on GET /fleet).
  const [viewer, setViewer] = useState<Viewer | null>(null);
  const fleet = !["#/apps/", "#/guardrails", "#/audit", "#/settings", "#/device", "#/approve", "#/no-access/"].some((screen) => hash.startsWith(screen));
  const away = useRef(false);
  // The name is read again on each return to the fleet: a rename under Guardrails is the heading on
  // the way back. The first read is the setup gate's, below.
  useEffect(() => {
    if (!fleet) { away.current = true; return; }
    if (!away.current) return;
    let live = true;
    getAuthConfig().then((config) => { if (live) { setOrg(config.org); setAuth(config.auth); } }, () => {}); // a failed read leaves things as they were
    return () => { live = false; };
  }, [fleet]);
  useEffect(() => {
    let live = true;
    const ask = async () => {
      const config = await getAuthConfig();
      if (live) { setOrg(config.org); setAuth(config.auth); }
      if (config.auth !== "oidc") return null;
      const record = await getPreferences();
      answered();
      return record.onboarded_at === null ? record : null;
    };
    // A failed read leaves `auth` null, not a guess of "oidc": guessing wrong on an auth-off host
    // asks and 404s again, the exact bug this card exists to not have. The other read above (on a
    // return to the fleet) sets it properly once it answers; this one only ever draws the dashboard.
    void ask().then((answer) => { if (live) setSetup(answer); }, () => { if (live) setSetup(null); });
    return () => { live = false; };
  }, []);
  // Asked once the host is past setup (every /api/v1 read but preferences is a 409 until then) and a
  // page with the rail is shown: the device and no-access pages have none, and a read there would be
  // one their own reads never asked for. Asked again on the next page while no answer has come, so a
  // read dropped or failed on the way is not the end of the foot.
  const railed = !hash.startsWith("#/device") && !hash.startsWith("#/approve") && !(hash.startsWith("#/no-access/") && hash !== "#/no-access/");
  // One read of the fleet's first row, again every 30 seconds while a railed page shows: who is signed in, for the
  // rail's foot (no name is no foot, never a guess), and what needs them, for the rail's count and the browser tab (D15).
  // ? while nothing is counted and some deployments couldn't be checked, and ? while the read fails: never the last
  // number, nor no count, which reads as all clear.
  const [needs, setNeeds] = useState<number | "?" | "unread">(0);
  useEffect(() => {
    if (setup !== null || !railed) return;
    let live = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const read = () => {
      getFleet("limit=1").then((page) => {
        if (!live) return;
        const counted = page.tiles.needs_you ?? 0;
        setNeeds(counted > 0 ? counted : (page.unchecked ?? 0) > 0 ? "?" : 0);
        if (auth === "oidc") setViewer(page.viewer);
      }, () => { if (live) setNeeds("unread"); }).finally(() => { if (live) timer = setTimeout(read, 30_000); });
    };
    read();
    return () => { live = false; clearTimeout(timer); };
  }, [setup, railed, auth]);
  useEffect(() => {
    const name = org ?? "Canyon";
    document.title = needs !== 0 ? S.needsTab.replace("<n>", needs === "unread" ? "?" : String(needs)).replace("<org>", name) : org === null ? "Canyon" : `${name} · Canyon`;
  }, [needs, org]);
  if (setup === undefined) return null;
  // Finishing writes the row, so the host has left the setup state and the fleet is served.
  if (setup !== null) return <Onboarding record={setup} done={() => setSetup(null)} />;
  // Guardrails is offered once the reader is known to be an admin: everyone is, on a host with sign-in off.
  return <RailContext value={{ org, viewer, admin: auth === "off" || viewer?.role === "admin", needs }}><Screen hash={hash} org={org} auth={auth} /></RailContext>;
}

function Screen({ hash, org, auth }: { hash: string; org: string | null; auth: "oidc" | "off" | null }) {
  // An id-less "#/apps/" or "#/no-access/" (no app named) is not a route either screen can serve:
  // it falls through to the fleet, exactly as an unrecognised hash does, rather than handing either
  // screen an id it would have to invent a request around.
  if (hash.startsWith("#/apps/")) {
    // #/apps/<id>/outbound is the link an egress refusal carries (contracts/routes.md, The check): the app's page,
    // at its Outbound card. An app id never holds a slash.
    const [id = "", section] = hash.slice("#/apps/".length).split("?")[0]!.split("/");
    if (id !== "") return <App key={hash} id={id} run={new URLSearchParams(hashQuery()).get("run")} auth={auth} outbound={section === "outbound"} />;
  }
  if (hash.startsWith("#/guardrails")) return <Guardrails />;
  if (hash.startsWith("#/audit")) return <AuditTrail key={hash} query={hashQuery()} />;
  if (hash.startsWith("#/settings")) return <Settings />;
  if (hash.startsWith("#/no-access/")) {
    const id = hash.slice("#/no-access/".length);
    if (id !== "") return <NoAccess key={hash} id={id} auth={auth} />;
  }
  if (hash.startsWith("#/device")) return <Device key={hash} code={new URLSearchParams(hashQuery()).get("code") ?? ""} org={org} />;
  if (hash.startsWith("#/approve")) return <Approve key={hash} code={new URLSearchParams(hashQuery()).get("code") ?? ""} org={org} />;
  return <Fleet org={org} />;
}

// #/apps/<id> is one app, keyed on the whole hash so a new id is a fresh page, and ?run= shows that
// run's findings in place of the newest's (an audit row's view →); #/guardrails is the
// org's; #/settings is the host's environments; #/audit?actor=&project=&range= is the org's trail, keyed on the hash so a new filter is a fresh
// read; #/device?code= is the CLI's approval page, keyed on the hash so a new code is a fresh
// page, and #/approve?code= the production approval page, the same way; #/no-access/<app> is where an app surface sends someone without access to it; anything
// else is the fleet. The id goes to the server as it is, and the server answers 404 for one it
// lacks. startsWith, not equality, so a query the guardrails screen grows still lands here.
// A 401 from any read replaces the whole tree with the sign-in page: the screen behind it unmounts,
// so nothing it read stays on screen or keeps polling. Signing in is a navigation, which lands on /,
// or back on the device, approval or no-access page it began on (SignIn's takeReturn).
// #/signin and #/setup-required are where a refused callback sends the browser. Only the callback
// navigates to #/signin — a 401 shows the sign-in page without moving the hash — so arriving on
// the route is itself the failure, and there is no marker in the URL to read.
export function Shell() {
  // Routing is hash-based (contracts/routes.md, Open group): the server has no fallback, so every
  // screen lives after the #. No hash is where the callback lands: a sign-in begun on the device or
  // a no-access page goes back there, and anything else opens on the fleet.
  const [hash, setHash] = useState(() => {
    if (window.location.hash === "") window.history.replaceState(null, "", takeReturn() ?? "#/fleet");
    return window.location.hash;
  });
  // Why the sign-in page shows: a sign-out the person chose, a session that ended, or none yet. The
  // server answers the same 401 for no session and a dead one, so only a tab whose read was answered
  // had a session to end.
  const [signedOut, setSignedOut] = useState<"signed-out" | "ended" | "none" | null>(null);
  const hadSession = useRef(false);
  useEffect(() => {
    const follow = () => setHash(window.location.hash);
    // A chosen sign-out sticks: a poll already on its way answers 401 after it.
    const out = (event: Event) => setSignedOut((was) => ((event as CustomEvent).detail === "signed-out" ? "signed-out" : was ?? (hadSession.current ? "ended" : "none")));
    window.addEventListener("hashchange", follow);
    window.addEventListener(UNAUTHENTICATED, out);
    return () => { window.removeEventListener("hashchange", follow); window.removeEventListener(UNAUTHENTICATED, out); };
  }, []);
  if (signedOut !== null) return <SignIn reason={signedOut === "none" ? undefined : signedOut} />;
  if (hash.startsWith("#/setup-required")) return <SetupNotice />;
  if (hash.startsWith("#/signin")) return <SignIn reason="failed" />;
  // Everything else is the dashboard, and the dashboard asks whether this host is set up first.
  // The two routes above do not: neither reads anything, and a host in setup is exactly where the
  // callback sends somebody to them.
  return <Dashboard hash={hash} answered={() => { hadSession.current = true; }} />;
}
