// Marks each mermaid diagram whose frame scrolls sideways by more than 4px, so its hint, "Scroll sideways to see
// all of it.", shows exactly then (src/styles/canyon.css). Checked when the page is read and whenever a frame
// changes size: a rotated phone, a resized window, fonts that load late.
const mark = (frame) => frame.parentElement.classList.toggle("scrolls", frame.scrollWidth > frame.clientWidth + 4);
const watch = () => {
  const observer = new ResizeObserver((entries) => entries.forEach((entry) => mark(entry.target)));
  document.querySelectorAll(".diagram-frame").forEach((frame) => observer.observe(frame));
};
if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", watch);
else watch();
