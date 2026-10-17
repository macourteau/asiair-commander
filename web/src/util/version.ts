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
