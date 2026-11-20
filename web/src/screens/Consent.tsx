import { useEffect, useState } from "react";
import { approveOAuthRequest, denyOAuthRequest, getOAuthRequest, said, signOut, type ApiError, type OAuthRequest } from "../api";
import { CONSENT } from "../strings";
import { ApprovingAs } from "../ui/ApprovingAs";
import { AuthCard, AuthHead, AuthNote } from "../ui/AuthCard";
import { Button } from "../ui/Button";

// An MCP client's consent page, #/oauth (design/AUTH.md §4.7): who asks, as whom, where Canyon sends the person back,
// and what it may do. A registration's name is the client's own word, and the page says so; a document's is shown
// with its URL's host. Allowing or refusing answers the client's redirect, which the browser follows. A request
// Canyon can't send back for, an unknown client or an unregistered redirect, arrives as ?invalid=, and only its
// refusal is shown: the page never follows a redirect it wasn't answered.
// leave is how the page hands the browser to the client's redirect, kept apart so a test sees where it went.
export const leave = { to: (url: string) => window.location.assign(url) };

export function Consent({ request, invalid, org }: { request: string; invalid: string | null; org: string | null }) {
  const [read, setRead] = useState<OAuthRequest | null>(null);
  const [gone, setGone] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const failed = (err: unknown) => {
    setSending(false);
    if ((err as ApiError).status === 404) setGone(true);
    else setFailure(said(err));
  };
  useEffect(() => {
    if (request !== "" && invalid === null) void getOAuthRequest(request).then(setRead, failed);
  }, [request, invalid]);
  const decide = (send: (id: string) => Promise<{ redirect: string }>) => () => {
    if (sending) return; // one answer: a second click while the first is on its way sends nothing
    setSending(true);
    void send(request).then((answer) => leave.to(answer.redirect), failed);
  };

  const reason = invalid === "client" || invalid === "redirect" ? CONSENT.reason[invalid] : null;
  if (invalid !== null || request === "") {
    return (
      <AuthCard org={org}>
        <AuthHead mark={{ tone: "critical", icon: "alert" }} title={CONSENT.invalid.replace("<reason>", reason ?? CONSENT.reason.client)} />
      </AuthCard>
    );
  }
  if (gone) {
    return (
      <AuthCard org={org}>
        <AuthHead mark={{ tone: "neutral", icon: "waiting" }} title={read === null ? CONSENT.expiredNoClient : CONSENT.expired.replace("<client>", read.client.name)} />
      </AuthCard>
    );
  }
  if (read === null) return <AuthCard org={org}>{failure !== null && <p className="text-md text-critical">{failure}</p>}</AuthCard>;
  const fill = (line: string) => line.replaceAll("<client>", read.client.name).replace("<display>", read.account.display).replace("<email>", read.account.email)
    .replace("<host>", read.host).replace("<redirect host>", read.redirect_host);
  return (
    <AuthCard org={org}>
      <div className="grid gap-5">
        <AuthHead title={fill(CONSENT.title)} line={fill(CONSENT.line)} />
        {read.client.verified
          ? <p className="text-md text-text-dim">{CONSENT.from.replace("<client id host>", read.client.host)}</p>
          : read.client.host === "" && <AuthNote tone="amber" icon="alert" title={CONSENT.unverified} />}
        <section aria-label="What it may do">
          <ul className="grid gap-1.5 rounded-lg border border-border px-3.5 py-3 text-md">
            {read.scopes.map((scope) => <li key={scope}>{CONSENT.scope[scope]}</li>)}
          </ul>
          <p className="mt-2 text-md text-text-dim">{CONSENT.only}</p>
        </section>
        <ApprovingAs approver={read.account} onSwitch={() => void signOut().catch(failed)} />
        <div className="grid gap-2.5 sm:grid-cols-2">
          <Button variant="primary" size="block" onClick={decide(approveOAuthRequest)}>{CONSENT.allow}</Button>
          <Button size="block" onClick={decide(denyOAuthRequest)}>{CONSENT.refuse}</Button>
        </div>
        {failure !== null && <p className="text-md text-critical">{failure}</p>}
      </div>
    </AuthCard>
  );
}
