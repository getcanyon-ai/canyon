// The portal's data, in memory for the fixture. The deployed demo seeds the same rows into Postgres.
export type Claim = { id: string; policyId: string; claimant: string; status: string; amountCents: number };
export type Policy = { id: string; orgId: string; holder: string; premiumCents: number };

const claims: Claim[] = [
  { id: "CLM-1041", policyId: "POL-88213", claimant: "R. Okafor", status: "approved", amountCents: 184000 },
  { id: "CLM-1042", policyId: "POL-88214", claimant: "M. Lindqvist", status: "review", amountCents: 92500 },
  { id: "CLM-1043", policyId: "POL-88220", claimant: "S. Haddad", status: "denied", amountCents: 0 },
  { id: "CLM-1044", policyId: "POL-88231", claimant: "J. Petrova", status: "approved", amountCents: 41200 },
];
const policies: Policy[] = [
  { id: "POL-88213", orgId: "meridian", holder: "R. Okafor", premiumCents: 12900 },
  { id: "POL-88214", orgId: "meridian", holder: "M. Lindqvist", premiumCents: 15400 },
];

export const db = {
  claims: {
    async search(q: string) {
      const needle = q.trim().toLowerCase();
      return needle ? claims.filter((c) => JSON.stringify(c).toLowerCase().includes(needle)) : claims;
    },
  },
  policies: {
    async forOrg(orgId?: string) {
      return orgId ? policies.filter((p) => p.orgId === orgId) : policies;
    },
  },
};
