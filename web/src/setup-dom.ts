// jsdom ships no AnimationEvent, and react-dom reads that absence at import time as an old browser
// and listens for `webkitAnimationEnd` instead — so onAnimationEnd would never fire under test.
// This is the class jsdom is missing and nothing else; it is its own module so that it runs before
// setup-tests.ts imports anything that pulls react-dom in. It carries no animationName, so a handler
// that ever filters on the animation's name would pass under test whatever it did in a browser.
if (!("AnimationEvent" in globalThis)) {
  Object.defineProperty(globalThis, "AnimationEvent", { value: class extends Event {}, configurable: true, writable: true });
}
