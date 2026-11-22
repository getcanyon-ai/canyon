// poll runs read now and again every ms while the tab shows, never a second read while one is in flight (#87: a fleet
// read that costs seconds was asked again two seconds after each answer, so one was always running): none while the
// tab is hidden, and one at once when it shows again or its window takes focus. It answers the function that stops it.
export function poll(read: () => Promise<void>, ms: number): () => void {
  let live = true;
  let running = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const tick = async () => {
    clearTimeout(timer);
    if (!live || running) return;
    if (document.visibilityState !== "hidden") {
      running = true;
      try {
        await read();
      } finally {
        running = false;
      }
    }
    if (live) timer = setTimeout(() => void tick(), ms);
  };
  const shown = () => { if (document.visibilityState !== "hidden") void tick(); };
  void tick();
  document.addEventListener("visibilitychange", shown);
  window.addEventListener("focus", shown);
  return () => {
    live = false;
    clearTimeout(timer);
    document.removeEventListener("visibilitychange", shown);
    window.removeEventListener("focus", shown);
  };
}
