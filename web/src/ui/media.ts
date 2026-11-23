import { useEffect, useLayoutEffect, useState, type RefObject } from "react";

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
// Again once the fonts are in, whose wider words moved it (#126): sideways alone, since by then the page may stand scrolled
// to the row a link named.
export const shownIntoView = (strip: string) => {
  const shown = () => document.querySelector<HTMLElement>(`nav[aria-label="${strip}"] [aria-current=page]`);
  shown()?.scrollIntoView?.({ inline: "center", block: "nearest" });
  void document.fonts?.ready.then(() => {
    const tab = shown();
    const nav = tab?.parentElement;
    if (!tab || !nav) return;
    const [at, within] = [tab.getBoundingClientRect(), nav.getBoundingClientRect()];
    nav.scrollLeft += at.left + at.width / 2 - (within.left + within.width / 2);
  });
};

const GUTTER = 16; // the page's own, on a phone
const LEAST = 160; // a menu opened near the window's foot is this tall all the same, and the page scrolls to it

// useInWindow keeps an open menu inside the window (#126): where it hangs from its button it is moved sideways until both
// its edges are in, and it is no taller than the room under it, scrolling inside itself past that (its class says
// overflow-y-auto). Placed before it is painted, and again when the window changes.
export function useInWindow(menu: RefObject<HTMLElement | null>, open: boolean) {
  useLayoutEffect(() => {
    const box = menu.current;
    if (!open || box === null) return;
    const place = () => {
      box.style.translate = "";
      box.style.maxHeight = "";
      const { left, right, top } = box.getBoundingClientRect();
      const width = document.documentElement.clientWidth; // never innerWidth, which a menu past the edge widens on a phone
      const over = Math.min(0, width - GUTTER - right);
      box.style.translate = `${Math.max(GUTTER - left, over)}px`;
      box.style.maxHeight = `${Math.max(LEAST, window.innerHeight - top - GUTTER)}px`;
      if (box.getBoundingClientRect().bottom > window.innerHeight) box.scrollIntoView?.({ block: "nearest" });
    };
    place();
    window.addEventListener("resize", place);
    return () => window.removeEventListener("resize", place);
  }, [menu, open]);
}
