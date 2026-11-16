// stillness waits until every surface stops changing: two reads 5 s apart agree four times running, 20 s of nothing,
// failing after within ms. A fresh stack is not still when it answers (egress-control's first ask writes its replica's row
// and moves the policy's), and an app's last calls reach its records after it is gone: a journey's steps declare neither.
import { diff, onCluster, quiet, snapshot } from "./surfaces";

export async function stillness(within: number, what: string) {
  let last = await snapshot();
  for (let still = 0, end = Date.now() + within; still < 4;) {
    if (Date.now() > end) throw new Error(`${what}: the stack kept changing for ${within / 1000} s`);
    await new Promise((resolve) => setTimeout(resolve, 5_000));
    const now = await snapshot();
    still = quiet(diff(last, now)) ? still + 1 : 0;
    last = now;
  }
}

// Playwright's globalSetup: once a run, before its first journey, on the Docker stack (the test cluster's surfaces are its own).
export default async function setup() {
  if (onCluster || process.env.E2E_CANYON_URL) return;
  await stillness(180_000, "before the first journey");
}
