import { useEffect, useState } from "react";

// Whether a media query matches now, following it as the window changes. jsdom has no matchMedia, so tests
// read the wide layout unless they stub it.
export function useMedia(media: string) {
  const [matches, setMatches] = useState(() => window.matchMedia?.(media).matches ?? false);
  useEffect(() => {
    const query = window.matchMedia?.(media);
    if (query === undefined) return;
    const follow = () => setMatches(query.matches);
    query.addEventListener("change", follow);
    return () => query.removeEventListener("change", follow);
  }, [media]);
  return matches;
}

// shownIntoView centres the tab shown in a strip of tabs, which on a phone scrolls sideways (#100's review). Called after
// the strip is drawn with every tab it will hold, so the last one comes fully in: Guardrails' and Settings' strips.
export const shownIntoView = (strip: string) =>
  document.querySelector(`nav[aria-label="${strip}"] [aria-current=page]`)?.scrollIntoView?.({ inline: "center", block: "nearest" });
