# addons-sample

An app connected to two services of its space, declared in `donkey.yaml`:

```yaml
addons:
  - type: postgres
  - type: pocketbase
```

`donkey deploy` provisions both for the space (each a Deployment with its
own disk), then starts the app with their bindings injected — `DATABASE_URL`
and the `PG*`/`JDBC_*` family for Postgres, `POCKETBASE_URL` and the admin
credentials for PocketBase. A second app in the space naming `postgres`
would join the same database, which is what a space is for.

This sample ships a `donkey.yaml` on purpose, which almost none of the others
do. Addons are the one thing donkey never provisions behind your back — a
detected `psycopg` import only earns a *suggestion*. So attached services are
exactly the case where the file is the feature: this folder is what a
repository looks like after someone ran `donkey addon add postgres` and
`donkey addon add pocketbase` and committed the result — and the test is that
cloning it and running `donkey deploy` rebuilds the whole arrangement.

The app itself is a guestbook that makes both bindings do real work — while
nothing in this folder contains a hostname, a port or a password. The page
(frontend) lists the notes and offers the form; the API (backend) stores
each note as a row in Postgres and logs it as a record in a PocketBase
collection, created on first use with the admin credentials from the
binding. The footer counts both, so the page is its own proof.

What a good run looks like:

- the first deploy says it is setting up `postgres` and `pocketbase` for the
  space, waits for them, and only then builds and starts the app
- signing the guestbook adds a note (Postgres) and an event (PocketBase),
  and both counts survive a redeploy (the services' disks, not the app's)
- `donkey addon list` shows both services, used by this app
