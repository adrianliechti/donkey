# Compatibility repair cases

These four apps must deploy successfully with the live AI builder. Their
laptop defaults or missing dependency declarations need a suitable build
recipe, startup command, or source repair.

| App | Challenge |
|---|---|
| `flask-loopback/` | Flask's development server binds to loopback. |
| `express-loopback/` | The server explicitly binds to `127.0.0.1`. |
| `streamlit-bare/` | No Procfile; the builder must discover the Streamlit startup command. |
| `no-manifest/` | Flask is imported without a dependency manifest. |

Run `task demo:broken` for CLI deployments, or `task verify:builder` for
plan review, deployment, and application checks through the gateway.
The latter includes these cases alongside all 19 regular samples and saves
plans, generated recipes, repair patches, and deployment streams in
`.build/e2e`. Source repairs happen in isolated build copies; the fixtures
remain unchanged. A build that gets the recipe right on the first attempt
is a success too.
