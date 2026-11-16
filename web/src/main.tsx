import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./app.css";
import { Shell } from "./screens/Shell";

const container = document.getElementById("root");
if (container === null) throw new Error("index.html has no #root");
// The page's one root, exported so a test that imports this module unmounts what it rendered before it ends (#2884).
export const root = createRoot(container);
root.render(<StrictMode><Shell /></StrictMode>);
