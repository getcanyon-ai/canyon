// I1, I2 and I5 (e2e/cluster/CASES.md, I — Isolation and security): the fixture's /probe and /fetch read through
// e2e-app's own address as alice. Every denied target is first shown to exist -- the peer's pod
// and Service, the server's and the executor's Services -- so a `timeout` is a NetworkPolicy drop,
// not a name that never resolved (the fixture never answers `timeout` for those). #487 holds I3, I4, P1.
import { expect, test, type Page } from "@playwright/test";
import { host } from "../harness/seed";
import { signIn } from "../harness/signin";
import { appUrl, bed, kube, podNamed, raw } from "../harness/cluster";

const app = "e2e-app";

async function result(page: Page, url: string): Promise<string> {
  await page.goto(url);
  await page.waitForURL((u) => u.origin === new URL(url).origin, { timeout: 60_000 });
  return (await page.locator("#result").innerText()).trim();
}

// servicePort is the one port of a Service, so a probed port is the port that really listens.
const probe = (page: Page, to: string) => result(page, `${appUrl(app, "preview")}/probe?to=${encodeURIComponent(to)}`);

function servicePort(namespace: string, name: string): number {
  const [svc] = kube(namespace, `service/${name}`) as unknown as { spec: { ports: { port: number }[] } }[];
  expect(svc, `no Service ${name} in ${namespace}`).toBeDefined();
  expect(svc!.spec.ports, `Service ${name}'s ports`).toHaveLength(1);
  return svc!.spec.ports[0]!.port;
}

test("I1 app A reaches nothing it should not", async ({ page }) => {
  await signIn(page, "alice");
  const peer = podNamed("canyon-apps", "e2e-peer-preview");
  expect(peer?.phase, "e2e-peer-preview is not running: I1 needs phase 2's peer deploy").toBe("Running");
  expect(servicePort("canyon-apps", "e2e-peer-preview")).toBe(4180);
  expect(servicePort("canyon", "canyon-server")).toBe(7777);
  expect(servicePort("canyon", "canyon-executor")).toBe(7778);
  const targets: [string, string][] = [
    ["e2e-peer-preview.canyon-apps.svc:4180", "timeout"],
    ["e2e-peer-preview.canyon-apps.svc:3000", "timeout"],
    ["canyon-server.canyon.svc:7777", "timeout"],
    ["canyon-executor.canyon.svc:7778", "timeout"],
    ["169.254.169.254:80", "timeout"],
    [`${new URL(host).hostname}:443`, "timeout"], // the floor: the internet is reached through Canyon's egress gateway alone (I5)
  ];
  for (const [to, want] of targets) expect.soft(await probe(page, to), `probe ${to}`).toBe(want);
});

test("I5 a call out leaves through Canyon, and is a row", async ({ page }) => {
  const session = await signIn(page, "alice");
  const name = new URL(host).hostname;
  expect(await result(page, `${appUrl(app, "preview")}/fetch?url=${encodeURIComponent(`${host}/`)}`), `fetch ${host}/`).toMatch(/^[1-5]\d\d$/);
  type Egress = { floor: string | null; envs: { preview: { via: string | null; destinations: { destination: string; port: number; last_outcome: string; last_protocol: string }[] } } };
  const read = async () => (await raw(session, "GET", `/api/v1/projects/${app}/egress`)).json as Egress;
  await expect.poll(async () => (await read()).envs.preview.destinations.find((d) => d.destination === name && d.port === 443), { timeout: 90_000, message: `no row of ${name}:443` })
    .toMatchObject({ last_outcome: "allowed", last_protocol: "connect" });
  expect(await read()).toMatchObject({ floor: "enforced", envs: { preview: { via: "canyon" } } });
});

test("I2 the apps database only on its own port", async ({ page }) => {
  await signIn(page, "alice");
  const apps = bed("E2E_APPS_DB_HOST");
  expect(await probe(page, `${apps}:5433`), `probe ${apps}:5433`).toBe("connected");
  expect.soft(await probe(page, `${apps}:5432`), `probe ${apps}:5432`).toBe("timeout");
  const canyon = bed("E2E_CANYON_DB_HOST");
  expect.soft(await probe(page, `${canyon}:5432`), `probe ${canyon}:5432`).toBe("timeout");
});
