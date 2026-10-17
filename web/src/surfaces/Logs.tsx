/**
 * Logs — one place to read Commander's own log AND the device logs it has pulled
 * per rig, so a postmortem starts here instead of an RDP session into the NAS.
 * Two views: the app log (filterable by level) and, per rig, the pulled device-log
 * files.
 */
import { useEffect, useState } from "react";
import {
  api,
  ApiError,
  type AppLogEntry,
  type LogPull,
  type RigProfile,
} from "@/api";
import {
  Card,
  EmptyState,
  Icon,
  Select,
  Spinner,
  Table,
  type Column,
} from "@/design";
import { absoluteTime, formatBytes, relativeTime } from "../util/format";
import "./surfaces.css";

type View = "app" | "device";

export function Logs() {
  const [view, setView] = useState<View>("app");
  return (
    <>
      <header className="surface-head">
        <div>
          <h1 className="surface-head__title">Logs</h1>
          <p className="surface-head__desc">
            Commander's own activity log, plus the device logs it has copied off
            each rig. Use these to see what happened and when.
          </p>
        </div>
      </header>

      <div className="toolbar">
        <div className="seg" role="group" aria-label="Which logs to show">
          <button
            type="button"
            aria-pressed={view === "app"}
            className="seg__btn"
            onClick={() => setView("app")}
          >
            <Icon name="logs" size={16} /> App log
          </button>
          <button
            type="button"
            aria-pressed={view === "device"}
            className="seg__btn"
            onClick={() => setView("device")}
          >
            <Icon name="rig" size={16} /> Device logs
          </button>
        </div>
      </div>

      {view === "app" ? <AppLog /> : <DeviceLogs />}
    </>
  );
}

const LEVELS = [
  { value: "", label: "All levels" },
  { value: "info", label: "Info and up" },
  { value: "warn", label: "Warnings and up" },
  { value: "error", label: "Errors only" },
];

const APP_LOG_MAX = 500;

const SSE_BACKOFF_MIN = 1000;
const SSE_BACKOFF_MAX = 30000;

type TailPhase = "connecting" | "open" | "reconnecting";

function AppLog() {
  const [level, setLevel] = useState("");
  const [entries, setEntries] = useState<AppLogEntry[] | null>(null);
  const [live, setLive] = useState(true);
  const [phase, setPhase] = useState<TailPhase>("connecting");

  // Live tail over a dedicated SSE stream (GET /api/app-logs/stream). The server
  // sends a filtered snapshot on connect, then each new line as it's logged.
  // A dropped stream that already opened is retried by EventSource itself; a
  // handshake that never opens (e.g. a 401/500 on connect) leaves readyState
  // CLOSED and is NOT retried by the browser, so we reconnect with backoff.
  useEffect(() => {
    if (!live) return;
    setEntries(null); // spinner until the snapshot arrives
    setPhase("connecting");

    let es: EventSource | null = null;
    let retry: ReturnType<typeof setTimeout> | null = null;
    let backoff = SSE_BACKOFF_MIN;
    let closed = false;

    const connect = () => {
      const qs = level ? `?level=${encodeURIComponent(level)}` : "";
      es = new EventSource(`/api/app-logs/stream${qs}`, { withCredentials: true });
      es.addEventListener("snapshot", (e) => {
        try {
          setEntries(JSON.parse((e as MessageEvent).data) as AppLogEntry[]);
        } catch {
          /* ignore a malformed frame */
        }
      });
      es.addEventListener("log", (e) => {
        try {
          const entry = JSON.parse((e as MessageEvent).data) as AppLogEntry;
          setEntries((prev) => [entry, ...(prev ?? [])].slice(0, APP_LOG_MAX));
        } catch {
          /* ignore */
        }
      });
      es.onopen = () => {
        backoff = SSE_BACKOFF_MIN;
        setPhase("open");
      };
      es.onerror = () => {
        if (closed) return;
        setPhase("reconnecting");
        // A CLOSED handshake won't self-retry; schedule our own with backoff.
        // While CONNECTING the browser is already retrying, so we just wait.
        if (es?.readyState === EventSource.CLOSED) {
          es.close();
          const jitter = Math.random() * 0.3 * backoff;
          const delay = Math.min(backoff, SSE_BACKOFF_MAX) + jitter;
          backoff = Math.min(backoff * 2, SSE_BACKOFF_MAX);
          retry = setTimeout(connect, delay);
        }
      };
    };

    connect();
    return () => {
      closed = true;
      if (retry) clearTimeout(retry);
      es?.close();
    };
  }, [level, live]);

  const status = !live
    ? "Paused"
    : phase === "open"
      ? "Live"
      : phase === "reconnecting"
        ? "Reconnecting…"
        : "Connecting…";

  return (
    <>
      <div className="toolbar">
        <label className="text-sm text-muted" htmlFor="app-log-level">
          Minimum level
        </label>
        <div style={{ maxWidth: 220 }}>
          <Select
            id="app-log-level"
            value={level}
            onChange={(e) => setLevel(e.target.value)}
            options={LEVELS}
          />
        </div>
        <div className="grow" />
        <a
          className="aac-btn aac-btn--secondary aac-btn--sm"
          href={api.appLogDownloadUrl}
          download="asiair-commander.log"
          title="Download the full current log file"
        >
          <Icon name="download" size={16} /> Download log
        </a>
        <button
          type="button"
          className={`live-toggle${live && phase === "open" ? " is-live" : ""}`}
          aria-pressed={live}
          onClick={() => setLive((v) => !v)}
          title={live ? "Pause live updates" : "Resume live updates"}
        >
          <span className="live-dot" aria-hidden />
          {status}
        </button>
      </div>

      {!entries ? (
        <div style={{ display: "grid", placeItems: "center", minHeight: "30vh" }}>
          <Spinner label="Loading log…" />
        </div>
      ) : entries.length === 0 ? (
        <Card pad>
          <EmptyState
            icon="logs"
            title="Nothing to show"
            description="No log lines at this level yet. Lower the minimum level to see more."
          />
        </Card>
      ) : (
        <div className="log-list" role="log" aria-label="Application log">
          {entries.map((e, i) => (
            <AppLogLine key={`${e.time}-${i}`} entry={e} />
          ))}
        </div>
      )}
    </>
  );
}

function AppLogLine({ entry }: { entry: AppLogEntry }) {
  const level = entry.level.toLowerCase();
  const cls =
    level.startsWith("err")
      ? "error"
      : level.startsWith("warn")
        ? "warn"
        : level.startsWith("info")
          ? "info"
          : "debug";
  const attrs = entry.attrs && Object.keys(entry.attrs).length > 0 ? entry.attrs : null;
  return (
    <div className="log-line">
      <time className="log-line__time" dateTime={entry.time} title={entry.time}>
        {formatIsoTime(entry.time)}
      </time>
      <span className={`log-line__level log-line__level--${cls}`}>{entry.level}</span>
      <div className="log-line__body">
        <div className="log-line__msg">{entry.message}</div>
        {attrs && (
          <div className="log-line__attrs">
            {Object.entries(attrs)
              .map(([k, v]) => `${k}=${formatAttr(v)}`)
              .join("  ")}
          </div>
        )}
      </div>
    </div>
  );
}

function DeviceLogs() {
  const [rigs, setRigs] = useState<RigProfile[] | null>(null);
  const [rigId, setRigId] = useState<string>("");
  const [pulls, setPulls] = useState<LogPull[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api
      .listRigs()
      .then((list) => {
        if (cancelled) return;
        setRigs(list);
        const first = list[0];
        if (first) setRigId((cur) => cur || first.id);
      })
      .catch((err) =>
        setError(err instanceof ApiError ? err.message : "Couldn't load rigs."),
      );
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (!rigId) return;
    let cancelled = false;
    setPulls(null);
    setError(null); // a fresh attempt clears any prior failure so a retry can succeed
    api
      .getRigLogs(rigId)
      .then((rows) => !cancelled && setPulls(rows))
      .catch(
        (err) =>
          !cancelled &&
          setError(err instanceof ApiError ? err.message : "Couldn't load device logs."),
      );
    return () => {
      cancelled = true;
    };
  }, [rigId]);

  const columns: Column<LogPull>[] = [
    { key: "type", header: "Log", cell: (p) => p.log_type || "—" },
    {
      key: "source",
      header: "On the device",
      cell: (p) => <span className="mono">{p.source_path}</span>,
      width: 220,
    },
    {
      key: "dest",
      header: "Saved to",
      cell: (p) => <span className="mono">{p.dest_path || "—"}</span>,
      width: 220,
    },
    { key: "size", header: "Size", cell: (p) => formatBytes(p.size), numeric: true },
    {
      key: "pulled",
      header: "Pulled",
      cell: (p) => (
        <span title={absoluteTime(p.pulled_at)}>{relativeTime(p.pulled_at)}</span>
      ),
      numeric: true,
    },
  ];

  // A failure to load the rig list leaves no selector to show, so it takes over
  // the view; a per-rig log-fetch failure renders inline below the toolbar (which
  // stays put) so the operator can pick another rig or retry.
  if (error && !rigs) {
    return (
      <Card pad>
        <p className="notice tone-problem" role="alert">
          <Icon name="problem" size={18} />
          <span>{error}</span>
        </p>
      </Card>
    );
  }
  if (!rigs) {
    return (
      <div style={{ display: "grid", placeItems: "center", minHeight: "30vh" }}>
        <Spinner label="Loading rigs…" />
      </div>
    );
  }
  if (rigs.length === 0) {
    return (
      <Card pad>
        <EmptyState
          icon="rig"
          title="No rigs configured"
          description="Add a rig on the Devices page, then its pulled logs will appear here."
        />
      </Card>
    );
  }

  return (
    <>
      <div className="toolbar">
        <label className="text-sm text-muted" htmlFor="device-log-rig">
          Rig
        </label>
        <div style={{ maxWidth: 260 }}>
          <Select
            id="device-log-rig"
            value={rigId}
            onChange={(e) => setRigId(e.target.value)}
            options={rigs.map((r) => ({ value: r.id, label: r.name || r.id }))}
          />
        </div>
      </div>

      {error ? (
        <Card pad>
          <p className="notice tone-problem" role="alert">
            <Icon name="problem" size={18} />
            <span>{error}</span>
          </p>
        </Card>
      ) : !pulls ? (
        <div style={{ display: "grid", placeItems: "center", minHeight: "20vh" }}>
          <Spinner label="Loading device logs…" />
        </div>
      ) : (
        <Table
          caption="Pulled device logs"
          columns={columns}
          rows={pulls}
          getKey={(p, i) => `${p.source_path}-${i}`}
          empty={
            <EmptyState
              icon="logs"
              title="No device logs pulled yet"
              description="Commander copies a rig's own logs when its profile has “Pull device logs” switched on."
            />
          }
        />
      )}
    </>
  );
}

/** RFC3339 → compact local time, e.g. "Jul 16, 22:14:07". */
function formatIsoTime(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  return new Date(t).toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function formatAttr(v: unknown): string {
  if (v == null) return "";
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}
