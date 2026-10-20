# ASIAIR Commander — contributor and agent notes

What the code does not tell you. Building and installing: [README.md](README.md), `make help`.

## Privacy and public-repo hygiene

This repository is public. Never put any of the following into code, comments, tests, fixtures,
docs, commit messages, PRs or issues:

- Names or handles of users, reporters or contributors (the copyright line in `LICENSE` and the
  installer is the one exception).
- Details from a user-reported problem: logs, paths, IP addresses, hostnames, device ids,
  locations, equipment lists, target or session names. Describe the behaviour generically, build
  the fixture synthetically, and refer to the report only by its public issue number.
- Real device identifiers (ASIAIR GUIDs, CPU ids, serials, Wi-Fi SSIDs). Use the synthetic values
  in [test/FIXTURES.md](test/FIXTURES.md), and add any value you invent there.
- Real LAN addresses or hostnames, absolute machine paths, private hosts or repositories, and
  secrets. Use RFC 5737/1918 addresses, `*.invalid` or `*.example` hosts, repo-relative paths.
- Real FITS headers with site or observer data. Generate fixtures with `internal/header/fitstest`.
  A fixture in `internal/header/testdata` carries no keyword on `header.PIIDenylist` and no
  `GPS*` keyword (`TestFixturesCarryNoPII`); never shrink that list to make a fixture pass.
- Version or incident anecdotes ("this broke in vX"). State the rationale instead.

Write code and docs as if they have always worked this way: no "new", "now" or "currently".

## Commits and pull requests

The release version **and** notes are derived from Conventional Commits on `main`. PRs are
squash-merged, so the **PR title** becomes the commit and must be conventional: `feat:` → minor,
`fix:` → patch, `feat!:`/`fix!:` or a `BREAKING CHANGE:` footer → major. Any other type
(`deps:`, `ci:`, `chore:`, `docs:`, `refactor:`, `test:`, `build:`, `perf:`), or a
non-conventional subject, releases silently as a patch when it touches shippable paths (`*.go`,
`go.mod`, `go.sum`, `web/**`, `Makefile`, `deploy/**`, `third_party/**`).

Land every change through a PR; a direct push to `main` never reaches the release notes. A
maintainer's own PR is armed with `gh pr merge --auto --squash` and lands once the required
`build-test` check passes. Never force-push `main`, and never merge another author's PR. A
design doc rides in the same PR as its implementation.

## Changes that can delete the wrong file

The delete path (`deleteOne`, `internal/rig/delete.go`), the device and media identity markers
(`internal/mediaid`, `internal/localfs/marker.go`, `internal/model/marker.go`) and reconciliation
(`internal/store/reconcile.go`) decide whether a frame on an ASIAIR is destroyed. A change there
needs a design doc in `docs/design/` first, holds auto-merge, and gets an independent
adversarial review of the actual diff confirming that the content-hash three-way check (device,
archive and recorded hash agree) is intact and that no caller of a removed symbol survives.
Mechanical one-file fixes are exempt.

### Listing completeness is a safety property

A verdict derived from a directory listing (*Safe to power off*, "everything pulled") is only as
trustworthy as the listing's completeness. SMB1 servers answer a TRANS2 FIND with as many bytes
as their send buffer holds and continue under the same MID; a client that reads one message per
MID gets a truncated, name-ordered prefix with no error, and a junk filter can hide that it was
short. `TestLargeDirectoryListsCompletely` guards this. A transport or enumeration change needs
an exact-count integration test, not a smoke test.

## Design docs

`docs/design/YYYY-MM-DD-<topic>.md`, indexed by [docs/design/README.md](docs/design/README.md),
each recording the decision and its rejected alternatives; significant work gets one before
code. The set: `overview`, `architecture`, `frame-lifecycle`, `delete-safety`,
`device-identity-and-media`, `library-reconciliation`, `routing-and-filing`, `remote-sites`,
`device-file-browser`, `asiair-hardware-notes`, `features`. Code comments cite `design <topic>
§N.N` (the filename without date and `.md`); Markdown elsewhere links the path plus §. Invariant
ids keep their values; I-series, AC and bare-number series are always qualified by their doc.

## Verify before pushing

`make check` mirrors the required `build-test` job step for step (SPA build and vitest, gofmt,
transcript immutability against `CHECK_BASE`, default `origin/main`, build, vet untagged and with
every tag CI uses, `go test -race`, the Windows cross-build); keep it and `ci.yml` in step.
`build-test` compiles the build-tagged tests but runs none; the `integration` job (real
Samba/NT1 and a real tunnel) runs only on pushes to `main` and tags. Run `make integration`
(needs Docker) whenever you change an interface those tests fake, such as `web.Supervisor`. The
recorded protocol transcripts (`internal/remote/proto/testdata/transcripts/`) and remote env
values (`internal/remote/tunnel/testdata/env/`) are immutable: a protocol or env format change
**adds** a file, and CI rejects any modification or deletion.

## Releases

`auto-release.yml` runs daily: when shippable code changed since the last `v*` tag it derives the
next version and dispatches `release.yml`, which builds the installer and **only then** tags,
publishes the release and builds the image. A tag pushed before a failed build would never get a
release, and the next run would read it as the latest, dropping its commits from every later
changelog; tagging last leaves nothing behind, so the next run retries the same version. Cut one
on demand with `gh workflow run auto-release.yml`. A bad release is fixed forward with a new
version; never delete-and-retag or move `:latest` to an older image, because watchtower would
auto-downgrade installs into a store that refuses to open a schema newer than its binary.

`update-astap-cli.yml` re-pins the ASTAP solver weekly (a PR for manual review) and mirrors the
pinned zips to an `astap-cli-<version>` prerelease. **Never delete those prereleases**: SourceForge
overwrites its version-less download in place and keeps one old CLI snapshot, so the mirror is
the only surviving copy of a superseded pin. `tools/fetch-astap` tries SourceForge, then the
mirror's public URL, then (only with `GH_TOKEN`/`GITHUB_TOKEN` set) the API asset endpoint,
checking each against the pinned SHA-256; no build needs a token.

## Dependencies

Dependabot updates Go modules, `web/` npm packages, Docker base images and Actions weekly;
`dependabot-automerge.yml` auto-merges every Actions bump and every non-major library or image
bump, and leaves majors for a human. `update-go-toolchain.yml` advances the Go toolchain pin.

When sibling Dependabot PRs touch the same files, GitHub disarms auto-merge on the others as soon
as the first merges, and nothing re-arms it. Sweep periodically: find green-but-open PRs with
`gh pr list --author 'app/dependabot'` and re-arm each with `gh pr merge <n> --auto --squash`,
skipping any labelled `tailcat-gate-failed` (its tunnel-library gate failed; it waits for a human).

## Verifying the UI

Do not hand back frontend work unchecked. `scripts/screenshots.mjs` (Playwright from the global
install, `NODE_PATH=$(npm root -g)`) captures every surface in its `SURFACES` list on mobile and
retina desktop, plus a mobile horizontal-overflow report; add each new surface to `SURFACES`.

Run the server for a pass with `make demo-up` and stop it with `make demo-down`, never with a
hand-rolled `cmd &` and `kill $!`: a backgrounded `(cd dir && cmd) &` chain records the
subshell's PID, so a later kill misses the server and it runs on unnoticed.

```
make demo-up SEED=seedremote                       # make spa, seed, build, start, wait for /healthz
OUT=<out-dir> AC_PASSWORD=<pw> TARGET=http://localhost:<port> \
  NODE_PATH=$(npm root -g) node scripts/screenshots.mjs
make demo-down                                     # SIGTERM, SIGKILL after 30s, verified gone
```

`demo-up` reseeds `DEMO_DATA` (default `tmp/demo`, per checkout, gitignored) from
`tools/<SEED>` (`SEED_ARGS=` for extra arguments; omit `SEED` to reuse the data dir), builds with
`TAGS` (default `demo`, empty for the seeders marked plain below), starts the server in the
background with `-pid-file`, logs to `$DEMO_DATA/server.log`, and returns only once `/healthz`
answers on the seeded port; it fails with the log tail otherwise, and refuses a second server on
the same data dir or a port already taken. `PORT=` moves it off the seeded port. `make
demo-status` reports it. `demo-down` exits non-zero unless it confirmed the process gone: report
that as a failure, with the PID, never as done. A demo build shuts itself down after 16 hours
(`MAX_LIFETIME=` sets `-demo-max-lifetime`, `0` for unlimited), so restart a server a long
session still needs.

Each seeder under `tools/` stages one situation; its README gives arguments and password. Ports:

| port | seeder | build |
| ---- | ------ | ----- |
| 8686 | `seedack` | plain |
| 8686 | `seeddemo` (the built-in default) | demo |
| 8787 | `seedmosaic`, `seedreal` (both `SEED_ARGS=<fits-root>`) | plain |
| 8788 | `seednight` | plain |
| 8789 | `seedscan` (stays in the foreground: `go run ./tools/seedscan tmp/demo` in its own terminal, then `make demo-up TAGS=`) | plain |
| 8790 | `seedremote` | demo |

A plain seeder stages offline or GUID-less rigs; a demo build answers every rig with an
in-memory device, which would bring them online and change the state they stage.

A `-tags demo` build's frames carry synthetic sky from `tools/starfield` (Gaia DR3 cones cached
under the user cache dir; a first run needs network), so previews, thumbnails and plate solves
work. `make screenshots` (`PORT=` to move it off 8686) regenerates the curated README images in
`docs/screenshots/`; never commit a rendered frame or a catalog file (Gaia is CC BY-NC 3.0 IGO).

State no URL reaches is driven by the seeder's `uipass.js`. API writes need an `Origin` header and
the `XSRF-TOKEN` cookie echoed as `X-CSRF-Token`. Full-page captures can exceed 10,000px; read the
`-viewport` variant. A long data-dir path inflates the overflow report (Settings renders roots
verbatim). Everything under `internal/web/dist` but its `.gitkeep` is gitignored build output;
a binary built without `make spa` serves `internal/web/placeholder.html` instead. Logic that can leave a component goes in `web/src/util/` with a vitest beside it.

## Code conventions

Match the surrounding style, keep diffs small, and comment the *why*. Every change carries tests;
a bug fix starts with a failing regression test. Handle errors at I/O, network and user-input
boundaries, log decision points, and never log secrets or user data.
