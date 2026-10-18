/**
 * Nav — the surface list. Rendered once; docked as a sidebar on desktop and used
 * as a slide-in drawer on mobile. Uses NavLink for active state (styled beyond
 * color: the active item also carries the accent + a persistent highlight, and
 * is exposed as aria-current so it's not signalled by color alone).
 */
import { useState } from "react";
import { NavLink } from "react-router";
import { Badge, Button, Icon, IconButton, useTheme, useToast, type IconName } from "@/design";
import { api, ApiError, useConnState, useServerInfo, useSnapshot } from "@/api";
import { themeSwitchView } from "../util/themeSwitch";
import { newerRelease, RELEASES_URL } from "../util/version";

interface NavItem {
  to: string;
  label: string;
  icon: IconName;
  end?: boolean;
}

const ITEMS: NavItem[] = [
  { to: "/", label: "Dashboard", icon: "dashboard", end: true },
  { to: "/feed", label: "Live feed", icon: "feed" },
  { to: "/devices", label: "Devices", icon: "devices" },
  { to: "/history", label: "History", icon: "clock" },
  { to: "/tools", label: "Tools", icon: "tools" },
  { to: "/files", label: "File browser", icon: "folder" },
  { to: "/settings", label: "Settings", icon: "settings" },
  { to: "/logs", label: "Logs", icon: "logs" },
  { to: "/alerts", label: "Alerts", icon: "alert" },
];

export interface NavProps {
  unackedAlerts: number;
  onLogout: () => void;
  /** Called after navigating (mobile: closes the drawer). */
  onNavigate?: () => void;
  /** Mobile drawer open state — adds the off-canvas "open" modifier. */
  open?: boolean;
}

const CONN_LABEL = {
  connecting: "Connecting…",
  open: "Live",
  reconnecting: "Reconnecting…",
  closed: "Offline",
} as const;

export function Nav({ unackedAlerts, onLogout, onNavigate, open = false }: NavProps) {
  const { theme, toggleTheme } = useTheme();
  const themeView = themeSwitchView(theme);
  const conn = useConnState();

  return (
    <nav className={`shell-nav${open ? " shell-nav--open" : ""}`} aria-label="Main">
      <div className="shell-brand">
        <Icon name="rig" size={24} className="shell-brand__mark" />
        <div className="shell-brand__text">
          <span className="shell-brand__name">
            ASIAIR
            <br />
            Commander
          </span>
          {__APP_VERSION__ && (
            <span className="shell-brand__version" title="UI build version">
              {__APP_VERSION__}
            </span>
          )}
          <NewerRelease />
        </div>
      </div>

      <ul className="shell-navlist list-reset">
        {ITEMS.map((item) => (
          <li key={item.to}>
            <NavLink
              to={item.to}
              end={item.end}
              onClick={onNavigate}
              className={({ isActive }) =>
                `shell-navlink${isActive ? " is-active" : ""}`
              }
            >
              <Icon name={item.icon} size={18} />
              <span className="shell-navlink__label">{item.label}</span>
              {item.to === "/alerts" && unackedAlerts > 0 && (
                <Badge
                  variant="problem"
                  aria-label={`${unackedAlerts} unacknowledged ${
                    unackedAlerts === 1 ? "alert" : "alerts"
                  }`}
                >
                  {unackedAlerts}
                </Badge>
              )}
            </NavLink>
          </li>
        ))}
      </ul>

      <div className="shell-nav__global">
        <GlobalPause />
      </div>

      <div className="shell-nav__spacer" />

      <div
        className={`shell-conn shell-conn--${conn}`}
        role="status"
        aria-label={`Live connection: ${CONN_LABEL[conn]}`}
      >
        <span className="shell-conn__dot" aria-hidden="true" />
        <span>{CONN_LABEL[conn]}</span>
      </div>

      <div className="shell-nav__footer">
        <Button
          variant="ghost"
          block
          className="shell-nav__theme"
          icon={<Icon name={themeView.icon} size={18} />}
          onClick={toggleTheme}
        >
          {themeView.label}
        </Button>
        <div className="shell-nav__row">
          <span className="text-muted text-sm">Signed in</span>
          <IconButton label="Sign out" icon={<Icon name="logout" />} onClick={onLogout} />
        </div>
      </div>
    </nav>
  );
}

// NewerRelease links to the latest release when the server reports one newer
// than the version it is running (design features §7). The server re-announces
// server_info when that changes, so it appears and disappears without a reload.
function NewerRelease() {
  const latest = newerRelease(useServerInfo());
  if (!latest) return null;
  const label = `Version ${latest} available`;
  return (
    <a
      className="shell-brand__update"
      href={RELEASES_URL}
      target="_blank"
      rel="noopener noreferrer"
      aria-label={`${label} (opens the release notes in a new tab)`}
      title={`${label}: view the release notes on GitHub`}
    >
      <Icon name="download" size={12} aria-hidden />
      <span>{latest} available</span>
    </a>
  );
}

// GlobalPause is the fleet-wide pause/resume switch (design overview §3: the
// global one for when you're doing something unusual). It toggles on whether every
// managed rig is already paused, read live from the snapshot.
function GlobalPause() {
  const snapshot = useSnapshot();
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const rigs = snapshot?.rigs ?? [];
  const allPaused = rigs.length > 0 && rigs.every((r) => r.paused);

  const toggle = async () => {
    setBusy(true);
    try {
      if (allPaused) {
        await api.resumeAll();
        toast.success("All rigs resumed.");
      } else {
        await api.pauseAll();
        toast.success("All rigs paused.");
      }
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "Couldn't reach the server.");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Button
      block
      variant={allPaused ? "primary" : "secondary"}
      icon={<Icon name={allPaused ? "play" : "pause"} size={16} />}
      loading={busy}
      disabled={rigs.length === 0}
      onClick={toggle}
    >
      {allPaused ? "Resume all rigs" : "Pause all rigs"}
    </Button>
  );
}
