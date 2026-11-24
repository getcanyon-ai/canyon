import { render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AuthCard } from "./AuthCard";

afterEach(() => vi.unstubAllGlobals());

// The lock beside the host says the connection is private: only over https (the expert review).
describe("the sign-in, consent and device card", () => {
  for (const [protocol, locks] of [["https:", 1], ["http:", 0]] as const) {
    it(`draws ${locks} lock beside the host over ${protocol}`, () => {
      vi.stubGlobal("location", { ...window.location, protocol, host: "canyon.example" });
      const { container } = render(<AuthCard org="Meridian">x</AuthCard>);
      expect(container.textContent).toContain("canyon.example");
      expect(container.querySelectorAll("p svg")).toHaveLength(locks);
    });
  }
});
