import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Pill } from "./Pill";

describe("Pill", () => {
  // One colour per status, and a word every time: the icon is drawn for sighted readers only.
  it("is the word in its tone, with the icon hidden from assistive tech", () => {
    render(<Pill tone="critical" icon="blocked">Blocked</Pill>);
    const pill = screen.getByText("Blocked");
    expect(pill.className).toContain("text-critical");
    expect(pill.className).toContain("bg-critical-dim");
    expect(pill.querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
  });

  it.each([["critical"], ["amber"], ["ok"], ["neutral"], ["beta"]] as const)("draws %s as its text on its own fill", (tone) => {
    render(<Pill tone={tone}>word</Pill>);
    const pill = screen.getByText("word");
    expect(pill.className).toContain(`text-${tone}`);
    expect(pill.className).toContain(`bg-${tone}-dim`);
  });

  it("draws no icon when given none", () => {
    render(<Pill tone="neutral">advisory</Pill>);
    expect(screen.getByText("advisory").querySelector("svg")).toBeNull();
  });
});
