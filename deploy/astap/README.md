# Bundling the ASTAP plate solver

Plate solving shells out to the external `astap_cli` binary (MPL-2.0). At runtime
the provisioner resolves it beside the service executable, then on `PATH`; there is
**no runtime download**. Instead the binary is fetched and bundled at **build time**
from a pinned upstream release.

- **`astap-cli.json`** — the pin: an ASTAP CLI `version` plus, per platform, the
  SourceForge folder/filename and the SHA-256 of the `.zip`. This is the single
  source of truth for which solver ships.
- **`tools/fetch-astap`** — downloads the pinned zip, **verifies the SHA-256**, and
  unzips `astap_cli` into a target dir. It fetches SourceForge's version-less
  "latest" URL first. When that no longer serves the pinned bytes (upstream rolled
  the release), it falls back to the project's own mirror: the `astap-cli-<version>`
  prerelease on `mirror_repo`, downloaded from its public `releases/download` URL
  without credentials. If that fails and `GH_TOKEN` or `GITHUB_TOKEN` is set, it
  tries once more through the GitHub API asset endpoint, which is how a mirror on a
  private fork is read. Every source is held to the same pinned digest, so a build
  of any pinned commit stays byte-reproducible after a new ASTAP release.

The mirror exists because SourceForge keeps no usable history: it overwrites the
version-less zips in place and its `older/` folder holds a single historical CLI
snapshot. A mirror prerelease is the only surviving copy of a superseded pin, so
**never delete one** — every build of a commit pinned to that version depends on it.

## Where it's wired

- **Docker** (`deploy/docker/Dockerfile`): the build stage runs
  `fetch-astap fetch linux/$TARGETARCH /out/astap`; the runtime stage copies it beside
  `/asiair-commander`. `--build-arg BUNDLE_ASTAP=0` skips it for offline builds.
- **Windows installer** (`make installer`): fetches `windows/amd64` into the NSIS
  `PAYLOAD` dir; `installer.nsi` bundles it (guarded by `!if /FileExists`, so a build
  without it still succeeds).

## Updating the pin

`.github/workflows/update-astap-cli.yml` runs weekly: it re-fetches every platform's
current upstream zip, and if any digest changed it rewrites `astap-cli.json`
(checksums + `version`, read from the binary's banner) and opens a PR **for manual
review** — no auto-merge, because a solver change warrants re-validating a real solve
(`ASTAP_BIN/CATALOG_DIR/FRAME=… go test ./internal/platesolve -run TestRealSolveE2E`)
before it ships. Every run also publishes the mirror prerelease for the version the
manifest pins, if it does not exist yet, and a push to `main` that changes the pin
triggers a run, so a pin is mirrored as soon as it lands. Run it by hand with
`go run ./tools/fetch-astap check` (report only) or `update` (rewrite the manifest).
