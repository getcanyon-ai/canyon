import { describe, expect, it } from "vitest";
import { oddAnswer } from "./answered";

// The rule internal/answered's Flagged is, case for case (contracts/routes.md, envs.<env>.answered).
describe("an answer to say beside readiness", () => {
  it("is no answer, or a status outside 2xx and 3xx but 401 and 404", () => {
    expect([199, 200, 302, 399, 400, 401, 403, 404, 500].map((status) => oddAnswer({ status, said: "" }))).toEqual(
      ["answered 199 at /", null, null, null, "answered 400 at /", null, "answered 403 at /", null, "answered 500 at /"]);
    expect(oddAnswer({ error: "refused" })).toBe("gave no answer at /");
    expect(oddAnswer(undefined)).toBeNull();
  });
});
