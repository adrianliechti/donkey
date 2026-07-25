# local-habits

A React app written the way people actually write them on a laptop — and
therefore a deploy that goes wrong in the ways deploys usually go wrong.
Nothing here is a mistake donkey should silently repair; it is a set of things
donkey should *notice and say out loud*.

| In the code | What happens on a cluster | What donkey should do |
|---|---|---|
| `fetch('http://localhost:3001/api/todos')` in `src/App.jsx` | The fetch runs in the visitor's **browser**, so `localhost` is their machine. The page loads, looks fine, and every call fails. | Warn, naming the file: browser code can't reach localhost. |
| `server.proxy` in `vite.config.js` | Dev-only. Never ships, never runs in the cluster. | Say nothing — a warning here would be a false alarm. |
| `.env` holding a key | The build context excludes it, so it never leaves the machine and never reaches the model. | Leave it out, and say which files it left behind. |
| `"start": "vite preview"` | Works, but binds whatever port it is told. | Use `$PORT`, which donkey always provides. |

## The point

The interesting case is the *first* row versus the *second*: both mention
`localhost:3001`, and only one of them matters. A check that flags every
occurrence of "localhost" trains people to ignore it.

## Fixing it, once donkey has told you

Either call a relative path and let a small server proxy it:

```js
fetch('/api/todos')          // same origin, no CORS, no cross-host login
```

…or, if the backend really is a separate public address, bake it in at build
time, which is the one place a bundler can see it:

```yaml
build:
  env:
    VITE_API_URL: https://api.example.com
```
