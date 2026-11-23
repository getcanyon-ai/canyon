import { expect } from "@playwright/test";
import { journey } from "../harness/factory";

// The two places a person meets agents (#99): what every agent is told, and how to connect one. Both read, and change
// nothing; the harness holds each to the page's layout rules at desktop and phone width.
journey("read what agents are told, then how to connect one", {
  steps: [
    {
      name: "open Guardrails › What agents are told",
      do: async (page) => { await page.goto("/#/guardrails/agents"); },
      see: async (page) => { await expect(page.getByRole("region", { name: "What agents are told" })).toBeVisible(); },
    },
    {
      name: "open Settings › Agents",
      do: async (page) => { await page.goto("/#/settings/agents"); },
      see: async (page) => { await expect(page.getByRole("region", { name: "Connect an agent" })).toBeVisible(); },
    },
  ],
});
