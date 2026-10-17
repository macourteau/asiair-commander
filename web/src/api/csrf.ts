/**
 * Double-submit CSRF: the backend mints a per-session token and delivers it in a
 * readable (non-HttpOnly) `XSRF-TOKEN` cookie. The SPA echoes it in the
 * `X-CSRF-Token` header on every mutating request; cross-site forms cannot read
 * the cookie, and the Host/Origin allowlist backstops DNS-rebinding (design
 * architecture §8).
 */

const COOKIE_NAME = "XSRF-TOKEN";
export const CSRF_HEADER = "X-CSRF-Token";

/** Reads the current CSRF token from the cookie, or "" if not present yet. */
export function readCsrfToken(): string {
  const target = COOKIE_NAME + "=";
  for (const part of document.cookie.split(";")) {
    const c = part.trim();
    if (c.startsWith(target)) {
      return decodeURIComponent(c.slice(target.length));
    }
  }
  return "";
}
