import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Icon } from "./Icon";

describe("Icon", () => {
  // Decorative everywhere: the word beside it carries the meaning.
  it("draws a hidden line icon at the size asked", () => {
    const svg = render(<Icon name="passed" size={20} />).container.querySelector("svg")!;
    expect(svg.getAttribute("aria-hidden")).toBe("true");
    expect(svg.getAttribute("width")).toBe("20");
    expect(svg.getAttribute("fill")).toBe("none");
  });

  it("draws the live dot solid", () => {
    expect(render(<Icon name="live" />).container.querySelector("svg")!.getAttribute("fill")).toBe("currentColor");
  });
});
