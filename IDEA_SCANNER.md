# Scanner ideas

Implemented behavior and configuration: [README](README.md#vulnerability-scanning).

- Add a manual rescan action and scan history with newly discovered findings.
- Triage new runtime findings against the source commit that built the image.
- Add security issues to the fleet’s attention list, separate from app health.
- Rebuild the last deployed source commit directly from the console.
- Feed previous scan findings into the next generated Dockerfile.
- Scan managed services and control-plane images, with upgrade guidance.
- Scan source uploads for secrets before sending them to the assistant.
- Scan Git history for secrets; keep credential verification opt-in.
- Use `govulncheck` reachability evidence when triaging Go applications.
- Mark scan results stale when the vulnerability database is too old.
- Measure scan cost and tune policy defaults from real deployment results.
