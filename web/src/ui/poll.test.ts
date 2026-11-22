import { afterEach, describe, expect, it, vi } from "vitest";
import { poll } from "./poll";

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("poll", () => {
  it("reads now and every interval after, never twice at once, none while hidden, at once when shown or focused", async () => {
    vi.useFakeTimers();
    let release: () => void = () => {};
    let reads = 0;
    const stop = poll(() => { reads++; return new Promise<void>((done) => { release = done; }); }, 10_000);
    expect(reads).toBe(1);
    window.dispatchEvent(new Event("focus")); // a read in flight: none more
    document.dispatchEvent(new Event("visibilitychange"));
    expect(reads).toBe(1);
    await vi.advanceTimersByTimeAsync(30_000); // still in flight: the interval waits for it
    expect(reads).toBe(1);
    release();
    await vi.advanceTimersByTimeAsync(9_999);
    expect(reads).toBe(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(reads).toBe(2);
    release();
    const hidden = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    await vi.advanceTimersByTimeAsync(60_000);
    expect(reads).toBe(2);
    hidden.mockReturnValue("visible");
    window.dispatchEvent(new Event("focus"));
    await vi.advanceTimersByTimeAsync(0);
    expect(reads).toBe(3);
    release();
    stop();
    await vi.advanceTimersByTimeAsync(60_000);
    window.dispatchEvent(new Event("focus"));
    expect(reads).toBe(3);
  });
});
