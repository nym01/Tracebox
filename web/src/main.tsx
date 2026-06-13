import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./monaco-setup.ts"; // bundle Monaco locally (no CDN) — must run before the editor mounts
import App from "./App.tsx";
import "./index.css";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
