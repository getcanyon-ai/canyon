// poll runs read now and again every ms while the tab shows, never a second read while one is in flight (#87: a fleet
// read that costs seconds was asked again two seconds after each answer, so one was always running): none while the
// tab is hidden, and one at once when it shows again or its window takes focus. now reads at once, after the read in
// flight if there is one, so a change the person just made is read; stop ends it.
export function poll(read: () => Promise<void>, ms: number): { now: () => void; stop: () => void } {
  let live = true;
  let running = false;
  let again = false; // asked for while a read was in flight: read once more when it ends
  let timer: ReturnType<typeof setTimeout> | undefined;
  const tick = async () => {
    clearTimeout(timer);
    if (!live) return;
    if (running) {
      again = true;
      return;
    }
    if (document.visibilityState !== "hidden") {
      running = true;
      try {
        await read();
      } finally {
        running = false;
      }
    }
    if (live && again) {
      again = false;
      void tick();
    } else if (live) {
      timer = setTimeout(() => void tick(), ms);
    }
  };
  const shown = () => { if (document.visibilityState !== "hidden" && !running) void tick(); };
  void tick();
  document.addEventListener("visibilitychange", shown);
  window.addEventListener("focus", shown);
  return {
    now: () => void tick(),
    stop: () => {
      live = false;
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", shown);
      window.removeEventListener("focus", shown);
    },
  };
}
