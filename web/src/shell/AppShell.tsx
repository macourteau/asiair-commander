/**
 * AppShell — the authenticated layout. Owns the single EventsProvider (so the
 * SSE stream runs only while signed in), the responsive nav, and the mobile
 * drawer state. Renders the active surface via <Outlet>.
 */
import { useEffect, useRef, useState } from "react";
import { Outlet, useLocation, useNavigationType } from "react-router";
import { Badge, Icon, IconButton } from "@/design";
import { EventsProvider, useSnapshot } from "@/api";
import { useAuth } from "../auth/AuthContext";
import { NAV_ITEMS, Nav } from "./Nav";
import { VersionBanner } from "./VersionBanner";
import { unackedAlertsLabel } from "../util/labels";
import { APP_NAME, pageForPath, pageTitle } from "../util/pageTitle";

// Matches the shell.css breakpoint where the nav docks as a sidebar.
const DESKTOP_QUERY = "(min-width: 960px)";

export function AppShell() {
  return (
    <EventsProvider>
      <ShellLayout />
    </EventsProvider>
  );
}

function ShellLayout() {
  const snapshot = useSnapshot();
  const { logout } = useAuth();
  const location = useLocation();
  const navigationType = useNavigationType();
  const [drawerOpen, setDrawerOpen] = useState(false);
  const unacked = snapshot?.unacked_alerts ?? 0;

  const menuButton = useRef<HTMLButtonElement>(null);
  const firstRoute = useRef(true);
  const pendingRouteFocus = useRef(false);

  // Close the mobile drawer whenever the route changes.
  useEffect(() => {
    setDrawerOpen(false);
  }, [location.pathname]);

  // Each page names itself in the tab title, and an in-app navigation moves focus
  // to the new page's heading so a screen reader announces where it landed. A
  // page load or reload (a POP on mount) leaves focus alone; arriving from
  // sign-in is a navigation (REPLACE) and moves it.
  useEffect(() => {
    document.title = pageTitle(pageForPath(location.pathname, NAV_ITEMS));
    const skip = firstRoute.current && navigationType === "POP";
    firstRoute.current = false;
    if (!skip) pendingRouteFocus.current = true;
    // Keyed on the path alone: a search-param change (the file browser's place)
    // is not a new page and manages its own focus.
  }, [location.pathname]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => () => void (document.title = APP_NAME), []);

  // Esc closes the drawer.
  useEffect(() => {
    if (!drawerOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setDrawerOpen(false);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [drawerOpen]);

  // Widening past the breakpoint docks the nav; a drawer left open would keep
  // <main> inert behind a sidebar that is no longer modal.
  useEffect(() => {
    const mq = window.matchMedia(DESKTOP_QUERY);
    const onChange = () => mq.matches && setDrawerOpen(false);
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  // When the drawer closes (Esc, the scrim, the menu button, a nav link) focus
  // must not stay inside the now-hidden drawer: hand it back to the menu button.
  // Declared before the route-focus effect so a navigation's heading focus wins.
  const wasOpen = useRef(false);
  useEffect(() => {
    if (drawerOpen) {
      wasOpen.current = true;
      return;
    }
    if (!wasOpen.current) return;
    wasOpen.current = false;
    const active = document.activeElement;
    const nav = document.getElementById("main-nav");
    if (!active || active === document.body || nav?.contains(active)) {
      menuButton.current?.focus();
    }
  }, [drawerOpen]);

  // Move focus to the page heading once <main> is interactive again (it is inert
  // while the drawer is open, and a navigation can land before the drawer closes).
  useEffect(() => {
    if (!pendingRouteFocus.current || drawerOpen) return;
    pendingRouteFocus.current = false;
    focusPageHeading();
  }, [location.pathname, drawerOpen]);

  return (
    <div className="shell">
      <a className="shell-skip" href="#main-content">
        Skip to main content
      </a>

      {/* Mobile top bar */}
      <header className="shell-topbar">
        <IconButton
          ref={menuButton}
          label={drawerOpen ? "Close menu" : "Open menu"}
          icon={<Icon name={drawerOpen ? "close" : "menu"} />}
          onClick={() => setDrawerOpen((v) => !v)}
          aria-expanded={drawerOpen}
          aria-controls="main-nav"
        />
        <span className="shell-topbar__brand">
          <Icon name="rig" size={20} />
          ASIAIR Commander
        </span>
        <div style={{ flex: "1 1 auto" }} />
        {unacked > 0 && (
          <Badge variant="problem" srLabel={unackedAlertsLabel(unacked)}>
            {unacked}
          </Badge>
        )}
      </header>

      {drawerOpen && (
        <div
          className="shell-drawer-scrim"
          onClick={() => setDrawerOpen(false)}
          aria-hidden="true"
        />
      )}

      {/* Sidebar on desktop; off-canvas drawer on mobile. */}
      <Nav
        unackedAlerts={unacked}
        onLogout={logout}
        onNavigate={() => setDrawerOpen(false)}
        open={drawerOpen}
      />

      {/* While the mobile drawer is open it is modal: the page behind it is inert. */}
      <main className="shell-main" id="main-content" tabIndex={-1} inert={drawerOpen}>
        <VersionBanner />
        <div className="shell-content">
          <Outlet />
        </div>
      </main>
    </div>
  );
}

/** How long to wait for a page that renders its heading only once its data loads. */
const HEADING_WAIT_MS = 5000;

/**
 * Focus the routed page's h1 without scrolling. A page still loading has no h1
 * yet: focus <main> meanwhile and move to the h1 when it renders, unless the
 * user has moved focus elsewhere by then.
 */
function focusPageHeading() {
  const main = document.getElementById("main-content");
  if (!main) return;
  const focusHeading = () => {
    const h1 = main.querySelector<HTMLElement>(".shell-content h1");
    if (!h1) return false;
    if (!h1.hasAttribute("tabindex")) h1.setAttribute("tabindex", "-1");
    h1.focus({ preventScroll: true });
    return true;
  };
  if (focusHeading()) return;
  main.focus({ preventScroll: true });
  const observer = new MutationObserver(() => {
    if (document.activeElement !== main) return stop();
    if (focusHeading()) stop();
  });
  const timer = window.setTimeout(() => stop(), HEADING_WAIT_MS);
  function stop() {
    observer.disconnect();
    window.clearTimeout(timer);
  }
  observer.observe(main, { childList: true, subtree: true });
}
