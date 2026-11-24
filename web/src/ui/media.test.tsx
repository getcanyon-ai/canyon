import { act, render, screen } from "@testing-library/react";
import { useRef } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { shownIntoView, useInWindow } from "./media";

const rect = (left: number, top: number, width: number, height: number) => ({ left, top, width, height, right: left + width, bottom: top + height, x: left, y: top, toJSON: () => ({}) }) as DOMRect;

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  delete (document as { fonts?: unknown }).fonts;
  delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
});

// The fonts widen the strip's words after it was drawn (#126): the tab shown is centred again when they are in, sideways
// alone, since by then the page may stand scrolled to the row a link named.
describe("shownIntoView", () => {
  it("centres the tab again once the fonts are in, and never scrolls the page again", async () => {
    let arrive = () => {};
    Object.defineProperty(document, "fonts", { configurable: true, value: { ready: new Promise<void>((done) => { arrive = done; }) } });
    const scrolled = vi.fn();
    Element.prototype.scrollIntoView = scrolled;
    render(<nav aria-label="Settings"><a href="#/a">One</a><a href="#/b" aria-current="page">Databases</a></nav>);
    const nav = screen.getByRole("navigation");
    nav.getBoundingClientRect = () => rect(16, 100, 358, 40);
    screen.getByRole("link", { name: "Databases" }).getBoundingClientRect = () => rect(316, 100, 70, 40);
    shownIntoView("Settings");
    expect(scrolled).toHaveBeenCalledTimes(1);
    expect(nav.scrollLeft).toBe(0);
    await act(async () => { arrive(); await Promise.resolve(); });
    expect(nav.scrollLeft).toBe(156); // its middle, 351, to the strip's, 195
    expect(scrolled).toHaveBeenCalledTimes(1);
  });
});

function Menu({ open }: { open: boolean }) {
  const box = useRef<HTMLDivElement>(null);
  useInWindow(box, open);
  return <div ref={box} data-testid="menu" />;
}

// A menu hung from its button stays in the window (#126): moved off either edge, no taller than the room under it.
describe("useInWindow", () => {
  const placed = (at: DOMRect) => {
    vi.spyOn(document.documentElement, "clientWidth", "get").mockReturnValue(390);
    vi.stubGlobal("innerHeight", 600);
    vi.spyOn(Element.prototype, "getBoundingClientRect").mockReturnValue(at);
    render(<Menu open />);
    return screen.getByTestId("menu").style;
  };
  it("comes back from the right edge", () => { expect(placed(rect(264, 200, 200, 120)).translate).toBe("-90px"); });
  it("comes back from the left edge, which no scroll width shows", () => { expect(placed(rect(-108, 200, 200, 120)).translate).toBe("124px"); });
  it("stays where it hangs when it fits", () => { expect(placed(rect(100, 200, 200, 120)).translate).toBe("0px"); });
  it("is no taller than the room under it", () => { expect(placed(rect(100, 200, 200, 900)).maxHeight).toBe("384px"); });
  it("keeps a height to read near the window's foot, where the page scrolls to it", () => { expect(placed(rect(100, 560, 200, 900)).maxHeight).toBe("160px"); });
  it("is left alone while closed", () => {
    render(<Menu open={false} />);
    expect(screen.getByTestId("menu").style.maxHeight).toBe("");
  });
});
