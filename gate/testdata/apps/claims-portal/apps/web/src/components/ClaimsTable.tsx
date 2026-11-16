"use client";

import { useCallback, useEffect, useState } from "react";
import type { Claim } from "@/lib/db";

const money = (cents: number) =>
  (cents / 100).toLocaleString("en-US", { style: "currency", currency: "USD" });

const STATUS_LABEL: Record<string, string> = {
  approved: "Approved",
  review: "In review",
  denied: "Denied",
};

export function ClaimsTable() {
  const [query, setQuery] = useState("");
  const [claims, setClaims] = useState<Claim[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const refetch = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await fetch(`/api/claims?q=${encodeURIComponent(query)}`);
      if (!res.ok) throw new Error(`claims request failed: ${res.status}`);
      setClaims(await res.json());
    } catch (e) {
      setError(e instanceof Error ? e.message : "could not load claims");
    } finally {
      setLoading(false);
    }
  }, [query]);

  const rows = claims.map((claim) => (
    <tr key={claim.id}>
      <td>{claim.id}</td>
      <td>{claim.policyId}</td>
      <td>{claim.claimant}</td>
      <td>{STATUS_LABEL[claim.status] ?? claim.status}</td>
      <td className="num">{money(claim.amountCents)}</td>
    </tr>
  ));

  const header = (
    <tr>
      <th>Claim</th>
      <th>Policy</th>
      <th>Claimant</th>
      <th>Status</th>
      <th className="num">Amount</th>
    </tr>
  );

  const status = loading
    ? "Loading…"
    : error ?? `${claims.length} claim${claims.length === 1 ? "" : "s"}`;

  // Filtering happens server-side; the endpoint returns the matching rows.
  // Keep the table in step with the search box.
  useEffect(() => { refetch(); }, [query]);

  return (
    <section className="claims">
      <label>
        Search claims
        <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Claim, policy or claimant" />
      </label>
      <p className="status">{status}</p>
      <table>
        <thead>{header}</thead>
        <tbody>{rows}</tbody>
      </table>
    </section>
  );
}
