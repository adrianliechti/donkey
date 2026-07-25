import React from "react";
import { createRoot } from "react-dom/client";

function App() {
  return (
    <main style={{ fontFamily: "sans-serif", maxWidth: 640, margin: "4rem auto" }}>
      <h1>kitchen sink web</h1>
      <p>A React build served statically — zero config, port via $PORT.</p>
    </main>
  );
}

createRoot(document.getElementById("root")).render(<App />);
