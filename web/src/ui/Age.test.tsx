import { describe, expect, it } from "vitest";
import { age, ago as said } from "./Age";

const ago = (ms: number) => new Date(Date.now() - ms).toISOString();

describe("age", () => {
  it("reads seconds as a whole minute — nothing reads 0m", () => expect(age(ago(30_000))).toBe("1m"));
  it("reads minutes", () => expect(age(ago(14 * 60_000))).toBe("14m"));
  it("reads hours", () => expect(age(ago(6 * 3_600_000))).toBe("6h"));
  it("reads days", () => expect(age(ago(3 * 86_400_000))).toBe("3d"));
  it("reads months past sixty days", () => expect(age(ago(70 * 86_400_000))).toBe("2mo"));
  it("clamps a future timestamp instead of going negative", () => expect(age(ago(-86_400_000))).toBe("1m"));
  it("has no answer for no timestamp", () => expect(age(null)).toBe("—"));

  it("says the span out loud for a column with room: one, many, and nothing to say", () => {
    expect(said(ago(86_400_000 + 60_000))).toBe("1 day ago");
    expect(said(ago(29 * 86_400_000))).toBe("29 days ago");
    expect(said(ago(70 * 86_400_000))).toBe("2 months ago");
    expect(said(null)).toBe("—");
  });
});
