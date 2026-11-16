import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Review, Stage, type StageKey } from "./Stage";

// One colour per status (the redesign): red blocked, amber waiting or an exception, green passed or live,
// grey not checked. Every stage is a word with an icon, never a bare glyph.
const TEN: [StageKey, string, string][] = [
  ["created", "Nothing uploaded", "text-neutral"],
  ["awaiting_review", "Awaiting review", "text-amber"],
  ["reviewing", "Reviewing", "text-neutral"],
  ["blocked", "Blocked", "text-critical"],
  ["approved", "Approved", "text-amber"],
  ["passed", "Passed", "text-ok"],
  ["building", "Building", "text-neutral"],
  ["preview", "Preview", "text-ok"],
  ["production", "Production", "text-ok"],
  ["failed", "Failed", "text-critical"],
];

describe("Stage", () => {
  for (const [stage, word, tone] of TEN) {
    it(`${stage} reads ${word} in ${tone}, with an icon`, () => {
      const { container } = render(<Stage stage={stage} />);
      expect(container.textContent).toBe(word);
      const pill = screen.getByText(word);
      expect(pill.className).toContain(tone);
      expect(pill.querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
    });
  }
  it("names the version before the pill", () => {
    const { container } = render(<Stage stage="blocked" version="v1.2" />);
    expect(container.textContent).toBe("v1.2Blocked");
  });

  // The fleet's Review column: once a version builds or runs, the newest run says what its review
  // said; a deploy is never a pass.
  it.each([
    ["preview", "clean", "Passed"], ["production", "blocked", "Blocked"], ["building", "running", "Reviewing"],
    ["preview", "failed", "Failed"], ["awaiting_review", "clean", "Awaiting review"], ["approved", "blocked", "Approved"],
    ["production", "never_reviewed", "Never reviewed"],
  ] as const)("Review of a %s version whose newest run is %s reads %s", (stage, gate, word) => {
    const { container } = render(<Review stage={stage} gate={gate} />);
    expect(container.textContent).toBe(word);
  });
});
