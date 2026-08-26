# addons-sample

An app connected to a service of its space, declared in `donkey.yaml`:

```yaml
addons:
  - type: postgres
```

`donkey deploy` provisions it for the space (a Deployment with its own disk),
then starts the app with its binding injected — `DATABASE_URL` and the
`PG*`/`JDBC_*` family. A second app in the space naming `postgres` would join
the same database, which is what a space is for.

This sample ships a `donkey.yaml` on purpose, which almost none of the others
do. Addons are the one thing donkey never provisions behind your back — a
detected `psycopg` import only earns a *suggestion*. So attached services are
exactly the case where the file is the feature: this folder is what a
repository looks like after someone ran `donkey addon add postgres` and
committed the result — and the test is that
cloning it and running `donkey deploy` rebuilds the whole arrangement.

The app itself is a guestbook that makes the binding do real work — while
nothing in this folder contains a hostname, a port or a password. The page
(frontend) lists the notes and offers the form; the API (backend) stores
each note as a row in Postgres. The footer shows the row count, so the page is
its own proof.

What a good run looks like:

- the first deploy says it is setting up `postgres` for the space, waits for
  it, and only then builds and starts the app
- signing the guestbook adds a note, and it survives a redeploy (the
  service's disk, not the app's)
- `donkey addon list` shows the service used by this app
