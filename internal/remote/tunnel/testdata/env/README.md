# Recorded remote env values

Each `v<N>.env` is a remote's `.env` text as a build writing env format version
N minted it, with comment lines naming the fingerprint the pairing must parse
to. `TestParseEnvReadsEveryOlderVersion` parses every recorded file on every
build, so a deployed `.env` of any older format stays readable.

**Recorded env values are immutable.** Never edit or delete a file here: a
remote's `.env` is never rewritten by an update, so each format in the field
stays exactly as recorded. A format change bumps `envFormatVersion` and adds a
new `v<N+1>.env`, minted by the build that introduces it, leaving every older
one in place.

Every value is minted on a test relay region (`InsecureForTests`), which no
shipped build accepts, so the keys it carries pair with nothing real.
