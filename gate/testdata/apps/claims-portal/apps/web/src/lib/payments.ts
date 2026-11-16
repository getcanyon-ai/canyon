import Stripe from "stripe";

// Payments for the checkout panel: an adjuster settles an approved claim by card.

export type CheckoutItem = { claimId: string; amountCents: number };

export async function createCheckout(items: CheckoutItem[]) {
  const total = items.reduce((sum, item) => sum + item.amountCents, 0);
  const intent = await stripe.paymentIntents.create({ amount: total, currency: "usd" });
  return intent.client_secret;
}

// Fixture: not a real key — it only has the shape of one so the gate can find it.
const stripe = new Stripe("sk_live_51MZq8fFIXTUREFAKEKEY07Kx2");
