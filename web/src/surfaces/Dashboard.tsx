/**
 * Dashboard — the morning-glance board. Live from the SSE snapshot (latest-wins).
 * Each rig gets a card whose HERO is the "safe to power off" status, rendered
 * EXACTLY as the backend computed it (safe / working / unknown) with color, icon,
 * AND text. A separate line states the different fact of whether the device is
 * drained vs. how many originals still sit on it.
 *
 * Per-rig active alerts come from GET /api/alerts, refetched whenever the
 * snapshot's unacked count changes or the stream reconnects (a dropped
 * alert.raised self-heals this way — design architecture §8).
 */
import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router";
import {
  api,
  useEventSubscription,
  useServerInfo,
  useSnapshot,
  type Alert,
  type RigStatus,
} from "@/api";
import {
  Badge,
  Card,
  EmptyState,
  Icon,
  Spinner,
  StatusPill,
} from "@/design";
import { rigStateView, safeToPowerOffView, showMonitoringPill } from "@/design";
import { absoluteTime, formatBytes, formatUptime, relativeTime } from "../util/format";
import { showSafeAsOf } from "../util/safeAsOf";
import { TransferProgress } from "./TransferProgress";
import { CleanupReadout } from "./CleanupReadout";
import { isWorking } from "../util/rigActivity";
import { MonitoringPill } from "./MonitoringPill";
import { NextFrame } from "./NextFrame";
import { WaitingToSettle } from "./WaitingToSettle";
import { RemotePill, RigRemoteLines } from "./RemoteRig";
import { rigRemoteView } from "../util/remoteRig";
import "./dashboard.css";

export function Dashboard() {
  const snapshot = useSnapshot();
  const alertsByRig = useUnackedAlertsByRig(snapshot?.unacked_alerts ?? 0);

  if (!snapshot) {
    return (
      <div style={{ display: "grid", placeItems: "center", minHeight: "40vh" }}>
        <Spinner label="Connecting to Commander…" />
      </div>
    );
  }

  const { rigs, nas_free, bytes_last_12h, generated_at } = snapshot;

  return (
    <>
      <header className="dash-head">
        <h1 className="dash-head__title">Tonight</h1>
        <div className="dash-head__meta">
          <span>
            NAS free <span className="mono">{formatBytes(nas_free)}</span>
          </span>
          <span>
            Transferred (12h){" "}
            <span className="mono">{formatBytes(bytes_last_12h)}</span>
          </span>
          <span>
            {rigs.length} {rigs.length === 1 ? "rig" : "rigs"}
          </span>
          <Uptime />
          {generated_at > 0 && (
            <span>
              Updated{" "}
              <span className="mono" title={absoluteTime(generated_at)}>
                {relativeTime(generated_at)}
              </span>
            </span>
          )}
        </div>
      </header>

      {rigs.length === 0 ? (
        <Card pad>
          <EmptyState
            icon="devices"
            title="No rigs yet"
            description="Add your ASIAIR so Commander can start pulling frames each night."
            action={
              <Link to="/devices" className="aac-btn aac-btn--primary">
                Go to Devices
              </Link>
            }
          />
        </Card>
      ) : (
        <ul className="dash-grid list-reset">
          {rigs.map((rig) => (
            <li key={rig.id}>
              <RigCard rig={rig} alerts={alertsByRig.get(rig.id) ?? []} />
            </li>
          ))}
        </ul>
      )}
    </>
  );
}

// Uptime shows how long the server process has been running, derived from the
// start time announced in server_info and re-rendered each second. It owns its own
// ticker so only this span re-renders, not the whole dashboard. Renders nothing
// until the start time is known.
function Uptime() {
  const serverInfo = useServerInfo();
  const startedAt = serverInfo?.started_at ?? 0;
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    if (!startedAt) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [startedAt]);

  if (!startedAt) return null;
  const uptimeSec = now / 1000 - startedAt;
  if (uptimeSec < 0) return null; // client clock behind the server; skip until it catches up
  return (
    <span>
      Up <span className="mono">{formatUptime(uptimeSec)}</span>
    </span>
  );
}

export function RigCard({ rig, alerts }: { rig: RigStatus; alerts: Alert[] }) {
  const [open, setOpen] = useState(false);
  const safe = safeToPowerOffView(rig.safe_to_power_off, rig.power_off_reason);
  const state = rigStateView(rig.state, rig.requested, rig.run_until_idle);
  // While its remote's tunnel is down the card reads the remote's state, not the
  // rig's own (the worker's Offline would blame the rig).
  const remotePill = rigRemoteView(rig).pill;
  const detailId = `rig-detail-${rig.id}`;
  // A rig that has not completed a listing since the server started sends shares
  // as null; normalize so the guard and map below never dereference null.
  const shares = rig.shares ?? [];

  return (
    <Card as="article" className="rigcard" interactive aria-label={`Rig ${rig.name}`}>
      <div className="rigcard__top">
        <span className="rigcard__name">
          {rig.name}
          <RemotePill site={rig.site} remoteName={rig.remote_name} />
        </span>
        <div className="rigcard__status">
          {remotePill ? (
            <StatusPill view={remotePill} />
          ) : showMonitoringPill(rig.state, rig.requested, rig.run_until_idle, rig.draining) ? (
            <MonitoringPill
              nextPollAt={rig.next_poll_at}
              intervalSec={rig.poll_interval_sec}
            />
          ) : (
            <StatusPill view={state} />
          )}
        </div>
      </div>

      {/* HERO — safe to power off */}
      <div className={`rigcard__hero tone-${safe.tone}`}>
        <Icon name={safe.icon} size={26} className="rigcard__hero-icon" />
        <div>
          <div className="rigcard__hero-label">{safe.label}</div>
        </div>
      </div>
      {safe.help && <p className="rigcard__hero-help">{safe.help}</p>}
      <RigRemoteLines rig={rig} />
      {/* A paused rig's verdict is frozen at its last run's listing rather than
          computed fresh; dating it says so, but only when it actually came from
          one — not on the Unknown a missing listing produced. */}
      {showSafeAsOf(rig.paused, rig.safe_to_power_off, rig.power_off_reason, rig.safe_as_of) && (
        <p className="rigcard__hero-help">
          as of{" "}
          <span title={absoluteTime(rig.safe_as_of!)}>
            {relativeTime(rig.safe_as_of!)}
          </span>
        </p>
      )}
      {/* The generic "unknown" help above is deliberately vague; missing_warning
          names the specific reason the safe state was downgraded (frames gone
          from the NAS while a live source could still supply them). */}
      {rig.missing_warning && (
        <p className="rigcard__hero-help tone-problem">{rig.missing_warning}</p>
      )}

      {/* Drain — a different fact from "safe" */}
      {rig.drain && (
        <DrainRow drain={rig.drain} keepOriginals={rig.keep_originals} />
      )}

      {/* Active alerts (always visible on the glance) */}
      {alerts.length > 0 && (
        <div className="rigcard__alerts">
          <StatusPill
            tone="problem"
            icon="alert"
            label={`${alerts.length} active ${alerts.length === 1 ? "alert" : "alerts"}`}
          />
          <Link to="/alerts" className="text-sm">
            Review
          </Link>
        </div>
      )}

      {/* Frames tonight — the headline count, always visible */}
      <div className="rigcard__stats">
        <div>
          <div className="stat__label">Transferred tonight</div>
          <div className="stat__value">{rig.frames_tonight}</div>
        </div>
        {rig.listing_ts > 0 && (
          <div>
            <div className="stat__label">Last listed</div>
            <div className="stat__value" title={absoluteTime(rig.listing_ts)}>
              {relativeTime(rig.listing_ts)}
            </div>
          </div>
        )}
      </div>

      {/* Frames seen but still being written — a plain "it's alive" liveness line */}
      <WaitingToSettle count={rig.waiting_to_settle} />

      {/* When the device next produces a frame (distinct from the next-poll line) */}
      <NextFrame
        prediction={rig.prediction}
        online={rig.online}
        paused={rig.paused}
        working={isWorking(rig)}
      />

      {/* Mobile-only expander for the denser detail */}
      <button
        type="button"
        className="rigcard__expand"
        aria-expanded={open}
        aria-controls={detailId}
        onClick={() => setOpen((v) => !v)}
      >
        {open ? "Hide detail" : "More detail"}
        <Icon name="chevron-down" size={16} />
      </button>

      <div
        id={detailId}
        className={`rigcard__detail${open ? " rigcard__detail--open" : ""}`}
      >
        <TransferProgress current={rig.current} />
        <CleanupReadout cleanup={rig.cleanup} />

        {shares.length > 0 && (
          <div>
            <div className="stat__label" style={{ marginBottom: "var(--space-1)" }}>
              Device free space
            </div>
            <div className="rigcard__shares">
              {shares.map((s) => (
                <div className="share-row" key={s.share}>
                  <span className="share-row__name truncate" title={s.share}>
                    {s.share}
                  </span>
                  <span className="share-row__free">{formatBytes(s.free)}</span>
                </div>
              ))}
            </div>
          </div>
        )}

        {rig.last_error && (
          <p className="rigcard__error" role="status">
            <Icon name="problem" size={15} />
            <span>{rig.last_error}</span>
          </p>
        )}
      </div>
    </Card>
  );
}

function DrainRow({
  drain,
  keepOriginals,
}: {
  drain: string;
  keepOriginals: boolean;
}) {
  // A keep-originals rig never runs the delete pass: it frees no device space and
  // is never "drained", so the count reads as originals kept by design — no
  // "space freed" and no "in grace" (nothing is pending deletion).
  if (keepOriginals) {
    return (
      <div className="rigcard__drain tone-neutral">
        <Icon name="share" size={16} style={{ color: "var(--tone-fg)" }} />
        <span>
          <span className="mono">{drain}</span> on the device
        </span>
        <Badge aria-label="This rig never deletes originals from the device">
          keep originals
        </Badge>
      </div>
    );
  }
  const drained = drain.trim().toLowerCase() === "drained";
  return (
    <div className={`rigcard__drain tone-${drained ? "safe" : "neutral"}`}>
      <Icon
        name={drained ? "safe" : "share"}
        size={16}
        style={{ color: "var(--tone-fg)" }}
      />
      <span>
        {drained ? (
          <>Device drained — space freed</>
        ) : (
          <>
            <span className="mono">{drain}</span> still on the device
          </>
        )}
      </span>
      {!drained && (
        <Badge aria-label="Originals kept during the grace period before deletion">
          in grace
        </Badge>
      )}
    </div>
  );
}

/** Unacked alerts grouped by rig id; refetches when the count changes or on reconnect. */
function useUnackedAlertsByRig(unackedCount: number): Map<string, Alert[]> {
  const [byRig, setByRig] = useState<Map<string, Alert[]>>(new Map());

  const refetch = useCallback(async () => {
    try {
      const alerts = await api.listAlerts();
      const map = new Map<string, Alert[]>();
      for (const a of alerts) {
        if (a.acked_at !== 0) continue;
        const list = map.get(a.rig_id);
        if (list) list.push(a);
        else map.set(a.rig_id, [a]);
      }
      setByRig(map);
    } catch {
      // Non-fatal: the alert badge/count from the snapshot still shows; the
      // Alerts surface is the authoritative place to read them.
    }
  }, []);

  useEffect(() => {
    void refetch();
  }, [refetch, unackedCount]);

  useEventSubscription("reconnect", () => void refetch());

  return byRig;
}
