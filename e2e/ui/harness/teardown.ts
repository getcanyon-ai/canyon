// After the Docker projects: each journey's app is new, so the build caches the executor kept for it
// (contracts/executor.md, canyon-cache-<app>-build) are never read again. Left behind, 800 MB a
// promote, they filled the laptop's disk. Only those no container holds: an app still running keeps
// its own, and a refused rm would fail the run. On the test cluster the apps build in kpack: no Docker volumes.
import { execFileSync } from "node:child_process";

export default function teardown() {
  if (process.env.E2E_CANYON_URL) return;
  const volumes = execFileSync("docker", ["volume", "ls", "-q", "--filter", "dangling=true", "--filter", "name=^canyon-cache-ux-"], { encoding: "utf8" }).split("\n").filter(Boolean);
  if (volumes.length) execFileSync("docker", ["volume", "rm", ...volumes], { stdio: "inherit" });
}
