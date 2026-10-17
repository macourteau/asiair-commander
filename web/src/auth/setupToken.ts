/**
 * The one-time first-run token's intake path: the Windows installer launches
 * `<url>#setup_token=<token>` so the wizard's field arrives pre-filled, and the
 * value must survive from page load to the moment the operator submits.
 *
 * It is captured at app entry (main.tsx) rather than on the Setup surface's mount,
 * and held in a module value rather than storage:
 *
 *  - a fragment survives only until the first navigation, and Setup renders behind
 *    the auth bootstrap plus a possible redirect, so reading it there loses it;
 *  - localStorage/sessionStorage would outlive the window the token is good for,
 *    leaving a live credential behind in a browser profile.
 */

/** The fragment key the launcher writes; matches deploy/windows/installer.nsi. */
const FRAGMENT_KEY = "setup_token";

let captured = "";

/**
 * Take the token out of `location.hash` and erase it from the address bar, so it
 * is not left in the URL to be shoulder-surfed, bookmarked, or pasted into a bug
 * report. Safe to call once; later calls are no-ops.
 */
export function captureSetupToken(): void {
  const hash = window.location.hash.replace(/^#/, "");
  if (!hash) return;
  const params = new URLSearchParams(hash);
  const token = params.get(FRAGMENT_KEY);
  if (!token) return;

  captured = token.trim();
  params.delete(FRAGMENT_KEY);
  const rest = params.toString();
  window.history.replaceState(
    null,
    "",
    `${window.location.pathname}${window.location.search}${rest ? `#${rest}` : ""}`,
  );
}

/** The token captured at app entry, or "" when the operator must paste it. */
export function setupTokenFromUrl(): string {
  return captured;
}
