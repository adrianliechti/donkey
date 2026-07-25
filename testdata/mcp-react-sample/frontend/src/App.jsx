import React, { useEffect, useState } from "react";

export default function App() {
  const [backend, setBackend] = useState("checking…");

  useEffect(() => {
    // Same-origin call — the BFF (server.js) proxies it to the MCP backend
    // inside the cluster.
    fetch("/api/health")
      .then((r) => r.json())
      .then((d) => setBackend(d.ok ? "connected ✓" : "unhealthy"))
      .catch(() => setBackend("unreachable ✗"));
  }, []);

  return (
    <main style={{ fontFamily: "sans-serif", maxWidth: 640, margin: "4rem auto" }}>
      <h1>React frontend, deployed by donkey 🫏</h1>
      <p>
        You are behind the login gateway. The MCP backend (reached through
        this app's own server): <strong>{backend}</strong>
      </p>
    </main>
  );
}
