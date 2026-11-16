import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Actor } from "./Actor";

const person = (display: string) => ({ kind: "person" as const, subject: "u:1", display });

describe("Actor", () => {
  it("shortens a person to a first name and a last initial", () => {
    render(<Actor actor={person("Dana Whitfield")} />);
    expect(screen.getByText("Dana W.")).toBeDefined();
  });

  it("leaves a one-word name alone", () => {
    render(<Actor actor={person("dana")} />);
    expect(screen.getByText("dana")).toBeDefined();
  });

  it("keeps an agent's whole name and marks what it is", () => {
    const { container } = render(<Actor actor={{ kind: "agent", subject: "a:1", display: "Claude Code" }} />);
    expect(container.textContent).toBe("Claude Codeagent");
  });

  it("does not say agent twice when the name already does", () => {
    const { container } = render(<Actor actor={{ kind: "agent", subject: "agent:claude-code/dana", display: "Dana's agent" }} />);
    expect(container.textContent).toBe("Dana's agent");
  });

  it("falls back to the subject when there is no display name", () => {
    render(<Actor actor={person("")} />);
    expect(screen.getByText("u:1")).toBeDefined();
  });

  it("has an em dash for nobody, never a blank", () => {
    render(<Actor actor={null} />);
    expect(screen.getByText("—")).toBeDefined();
  });
});
