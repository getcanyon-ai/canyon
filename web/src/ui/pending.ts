import type { GatewayPending } from "../api";
import { S } from "../strings";

// pendingLine is what a person reads, in place of an outbound rule change's own confirmation, while a gateway replica
// hadn't taken it when the server stopped waiting (#2789); null when every replica had.
export const pendingLine = (pending: GatewayPending) =>
  pending === null ? null : pending === "unread" ? S.gatewaysUnread : S.gatewayPending.replace("<replica>", () => pending);
