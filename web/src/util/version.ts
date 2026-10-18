/**
 * updateAvailable compares the UI's own build version against the server's and
 * returns the server version when they differ — meaning the running tab is stale
 * and a reload would pull a fresh bundle — or null when they match or the check
 * can't apply. A "dev"/empty version on either side (local dev, an untagged
 * build) suppresses the comparison entirely, so those never nag. Plain
 * inequality, not "server is newer": a rollback leaves the tab equally stale, so
 * either direction of skew surfaces.
 */
export function updateAvailable(
  ui: string,
  server: string | null | undefined,
): string | null {
  const real = (v: string | null | undefined): v is string =>
    v != null && v !== "" && v !== "dev";
  if (!real(ui) || !real(server)) return null;
  return server !== ui ? server : null;
}

/** Where the nav's newer-release indicator links. */
export const RELEASES_URL =
  "https://github.com/macourteau/asiair-commander/releases/latest";

/**
 * newerRelease returns the published release the server reported as newer than
 * the version it is running, or null when there is none. The server does the
 * semver comparison and reports "" for none (including when the check is
 * switched off); this guards only against a payload that would make the
 * indicator lie — a missing field, a "dev"/unset running version, or a
 * "latest" equal to what is installed. It compares against the server's running
 * version, never the tab's __APP_VERSION__: the indicator is about what is
 * installed, and a stale tab is VersionBanner's job.
 */
export function newerRelease(
  info: { version?: string; latest_version?: string } | null | undefined,
): string | null {
  const running = info?.version;
  const latest = info?.latest_version;
  if (!running || running === "dev" || !latest || latest === running) return null;
  return latest;
}

/**
 * newerReleaseText words the nav's newer-release link: `name` is its accessible
 * name (the version, and that it leaves for a new tab, since the visible control
 * is an icon alone) and `tooltip` the hint shown on hover and keyboard focus.
 */
export function newerReleaseText(latest: string): { name: string; tooltip: string } {
  return {
    name: `Version ${latest} available, opens release notes in a new tab`,
    tooltip: `${latest} available — view release notes`,
  };
}
