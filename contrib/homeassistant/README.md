# reostream Home Assistant add-on repository

Home Assistant reads an add-on repository from the root of a git repository, so this
directory is published as its own repository,
`https://github.com/VoltTech21/reostream-homeassistant`. That is the URL to add in
Home Assistant; `reostream/DOCS.md` has the steps.

- `repository.yaml` names the repository.
- `reostream/` is the add-on: `config.yaml`, a `Dockerfile` built on the release image of
  the same version, `DOCS.md` (the add-on's Documentation tab), and its icon and logo.

When releasing, set `version` in `reostream/config.yaml` to the new release's version
(without the `v`) and copy this directory to that repository.
