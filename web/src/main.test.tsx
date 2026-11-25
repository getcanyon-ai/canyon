import { act, waitFor } from "@testing-library/react";
import { afterAll, expect, it, vi } from "vitest";

// Every read the app makes, and whether the test had ended when it made it. The stub stays for the whole file, so a read
// the app makes after the test is seen here rather than reaching the real fetch: one that did, and answered after the
// environment closed, rendered with no window (#2884).
const reads: { url: string; late: boolean }[] = [];
let ended = false;

it("comes back from the callback to the device page the sign-in began on", async () => {
  const answer = (status: number, body: unknown) => ({ ok: status < 300, status, json: async () => body }) as unknown as Response;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    reads.push({ url, late: ended });
    if (url === "/auth/config") return answer(200, { canyon: true, auth: "oidc", org: "Meridian Insurance", device: true });
    if (url === "/api/v1/preferences") return answer(200, { onboarded_at: "2026-09-16T09:00:00Z" });
    return answer(404, { error: "not_found", message: "not found" });
  }));
  document.body.innerHTML = '<div id="root"></div>';
  window.history.replaceState(null, "", "/");
  sessionStorage.setItem("canyon_return", "#/device?code=K7QP-4RXM");
  sessionStorage.setItem("canyon_return_at", String(Date.now())); // kept as the sign-in began, within its 10 minutes (#154)
  const { root } = await import("./main");
  await waitFor(() => expect(window.location.hash).toBe("#/device?code=K7QP-4RXM"));
  expect(sessionStorage.getItem("canyon_return")).toBeNull();
  act(() => root.unmount()); // what main rendered goes with the test: nothing of it reads or renders after
  ended = true;
});

// What the test rendered is gone when it ends: no read, and so no render, comes after it.
afterAll(async () => {
  await new Promise((resolve) => setTimeout(resolve, 50)); // the device page's own read, had it stayed mounted
  vi.unstubAllGlobals();
  expect(reads.filter((r) => r.late).map((r) => r.url)).toEqual([]);
});
