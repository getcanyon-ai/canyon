import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Dial } from "./Dial";

describe("Dial", () => {
  // Two rules may share a name (a custom rule named as a shipped one); their radios never share a group.
  it("gives each control its own radio group", () => {
    render(<><Dial name="Same" value="off" change={() => {}} /><Dial name="Same" value="blocking" change={() => {}} /></>);
    const [one, two] = screen.getAllByRole("radiogroup", { name: "Same" }).map((group) => (within(group).getAllByRole("radio")[0] as HTMLInputElement).name);
    expect(one).not.toBe(two);
  });
});
