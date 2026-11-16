import { AUTH } from "../strings";
import { Brand } from "../ui/Icon";

// #/setup-required: where the callback sends somebody refused only because nobody has finished
// installing this host (design/ONBOARDING.md §2). One sentence, no controls and nowhere to go —
// deliberately. "Try again" is the single instruction that cannot work here, so the screen
// does not offer it; the only way forward is to tell whoever installed Canyon.
export function SetupNotice() {
  return (
    <main className="flex min-h-screen flex-col items-center justify-center gap-4 px-4 text-center">
      <Brand />
      <p className="max-w-[440px] text-md text-text-dim">{AUTH.setupRequired}</p>
    </main>
  );
}
