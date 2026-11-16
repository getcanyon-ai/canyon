import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Button } from "./Button";

describe("Button", () => {
  it("renders a disabled button's reason beside it, never as a tooltip", () => {
    render(<Button disabled reason="The gate blocked this package.">Promote to production</Button>);
    const button = screen.getByRole("button", { name: "Promote to production" });
    expect(button.hasAttribute("disabled")).toBe(true);
    expect(button.hasAttribute("title")).toBe(false);
    expect(screen.getByText("The gate blocked this package.")).toBeDefined();
  });

  it("gives ghost its own weight, with no base weight to outrank it", () => {
    render(<Button variant="ghost">Details</Button>);
    const classes = screen.getByRole("button").className;
    expect(classes).toContain("font-medium");
    expect(classes).not.toContain("font-semibold");
  });
});
