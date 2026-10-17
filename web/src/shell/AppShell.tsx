/**
 * AppShell — the authenticated layout. Owns the single EventsProvider (so the
 * SSE stream runs only while signed in), the responsive nav, and the mobile
 * drawer state. Renders the active surface via <Outlet>.
 */
import { useEffect, useState } from "react";
import { Outlet, useLocation } from "react-router";
import { Badge, Icon, IconButton } from "@/design";
import { EventsProvider, useSnapshot } from "@/api";
import { useAuth } from "../auth/AuthContext";
import { Nav } from "./Nav";
import { VersionBanner } from "./VersionBanner";

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
  const [drawerOpen, setDrawerOpen] = useState(false);
  const unacked = snapshot?.unacked_alerts ?? 0;

  // Close the mobile drawer whenever the route changes.
  useEffect(() => {
    setDrawerOpen(false);
  }, [location.pathname]);

  // Esc closes the drawer.
  useEffect(() => {
    if (!drawerOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setDrawerOpen(false);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [drawerOpen]);

  return (
    <div className="shell">
      <a className="shell-skip" href="#main-content">
        Skip to main content
      </a>

      {/* Mobile top bar */}
      <header className="shell-topbar">
        <IconButton
          label={drawerOpen ? "Close menu" : "Open menu"}
          icon={<Icon name={drawerOpen ? "close" : "menu"} />}
          onClick={() => setDrawerOpen((v) => !v)}
          aria-expanded={drawerOpen}
        />
        <span className="shell-topbar__brand">
          <Icon name="rig" size={20} />
          ASIAIR Commander
        </span>
        <div style={{ flex: "1 1 auto" }} />
        {unacked > 0 && (
          <Badge
            variant="problem"
            aria-label={`${unacked} unacknowledged ${unacked === 1 ? "alert" : "alerts"}`}
          >
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

      <main className="shell-main" id="main-content">
        <VersionBanner />
        <div className="shell-content">
          <Outlet />
        </div>
      </main>
    </div>
  );
}
