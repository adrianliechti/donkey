# monorepo-root

A repository that is *not* an app. The app lives one level down, in
`services/api/`, next to the docs and CI config every real repository
carries.

## The challenge

Somebody drops this whole folder on the console's Deploy page, or uploads the
repository root instead of the service. Nothing at the top level is
deployable: no `donkey.yaml`, no `package.json`, no `requirements.txt` — only
a README and some docs.

Donkey should find the app rather than fail, say which folder it picked, and
build that. With more than one candidate it must ask instead of guessing:
add a second service here and the same upload should stop and name both.
