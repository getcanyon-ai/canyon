import { ClaimsTable } from "@/components/ClaimsTable";
import { CheckoutPanel } from "@/components/CheckoutPanel";

export default function Home() {
  return (
    <>
      <h1>Open claims</h1>
      <ClaimsTable />
      <h2>Settle</h2>
      <CheckoutPanel claimId="CLM-1041" amountCents={184000} />
    </>
  );
}
