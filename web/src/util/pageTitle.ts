/**
 * Per-page document titles. Each surface's tab title names the page first so a
 * screen reader announces where a navigation landed, and a list of open tabs
 * stays tellable apart.
 */
export const APP_NAME = "ASIAIR Commander";

export interface PageRoute {
  to: string;
  label: string;
  /** Match the path exactly (the index route), not as a prefix. */
  end?: boolean;
}

/** The label of the route `pathname` belongs to, or undefined when none matches. */
export function pageForPath(pathname: string, routes: readonly PageRoute[]): string | undefined {
  const path = pathname.length > 1 ? pathname.replace(/\/+$/, "") : pathname;
  return routes.find((r) =>
    r.end ? path === r.to : path === r.to || path.startsWith(`${r.to}/`),
  )?.label;
}

/** "<Page> · ASIAIR Commander", or the bare app name for an unnamed page. */
export function pageTitle(page: string | undefined): string {
  return page ? `${page} · ${APP_NAME}` : APP_NAME;
}
