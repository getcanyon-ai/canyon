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
