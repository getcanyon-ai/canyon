import type { Env } from "../api";

// What the app answered at / when it is an answer to say beside readiness (contracts/routes.md,
// envs.<env>.answered): no answer, or a status outside 2xx and 3xx but 401 and 404. null for any other or none.
export function oddAnswer(answered: Env["answered"]): string | null {
  if (answered === undefined) return null;
  if ("error" in answered) return "gave no answer at /";
  return (answered.status < 200 || answered.status >= 400) && answered.status !== 401 && answered.status !== 404 ? `answered ${answered.status} at /` : null;
}
