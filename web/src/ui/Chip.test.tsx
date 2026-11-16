import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { S } from "../strings";
import { Chip } from "./Chip";

const classesOf = (tone: "critical" | "high" | "medium" | "low" | "advisory" | "blocks" | "approved") =>
  render(<Chip tone={tone}>{tone}</Chip>).container.firstElementChild?.className ?? "";

describe("Chip", () => {
  it("draws both blocking severities red, medium amber, and low and advisory grey", () => {
    expect(classesOf("critical")).toContain("text-critical");
    expect(classesOf("high")).toContain("text-critical");
    expect(classesOf("blocks")).toContain("text-critical");
    expect(classesOf("medium")).toContain("text-amber");
    expect(classesOf("low")).toBe(classesOf("advisory"));
    expect(classesOf("low")).toContain("text-neutral");
  });

  it("draws an approved blocking finding as an exception, amber, never as passed", () => {
    expect(classesOf("approved")).toContain("text-amber");
    expect(classesOf("approved")).not.toContain("text-ok");
  });

  // design/EXPERIENCE.md D5 (#2384): Beta is a word in a hue no status uses, never the colour alone. What it
  // means is its title for a pointer, and read after the word by assistive tech, so keyboard and touch get it too.
  it("draws beta in its own tone, and says what beta means to a pointer and to a screen reader", () => {
    const chip = render(<Chip tone="beta">{S.beta}</Chip>).getByText(S.beta);
    expect(chip.className).toContain("text-beta");
    expect(chip.className).toContain("bg-beta-dim");
    expect(chip.getAttribute("title")).toBe(S.betaWhy);
    expect(chip.textContent).toBe(`${S.beta}: ${S.betaWhy}`);
  });

  // Inside a link or a group the sentence is their description, never part of their name.
  it("given an id, keeps what beta means out of the name around it, for its aria-describedby", () => {
    render(<a href="#/x" aria-describedby="why">Outbound <Chip tone="beta" why="why">{S.beta}</Chip></a>);
    expect(screen.getByRole("link", { name: `Outbound ${S.beta}`, description: S.betaWhy })).toBeDefined();
  });
});
