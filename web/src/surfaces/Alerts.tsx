/**
 * Alerts — the problem feed. Each alert is a severity (color + icon + TEXT, never
 * color alone), a plain-language category, the rig it belongs to, the message, and
 * when it happened, with an Acknowledge action. Live: refetches on the SSE
 * `alert.raised` event, whenever the snapshot's unacked count changes, and on a
 * stream reconnect (a dropped event self-heals — design architecture §8).
 */
import { useCallback, useEffect, useState } from "react";
import {
  api,
  ApiError,
  useEventSubscription,
  useSnapshot,
  type Alert,
} from "@/api";
import {
  Button,
  Card,
  EmptyState,
  Icon,
  Spinner,
  StatusPill,
  Toggle,
  severityView,
  useToast,
} from "@/design";
import { alertCategoryLabel } from "../util/labels";
import { absoluteTime, relativeTime } from "../util/format";
import "./surfaces.css";

export function Alerts() {
  const snapshot = useSnapshot();
  const toast = useToast();
  const [includeAcked, setIncludeAcked] = useState(false);
  const [alerts, setAlerts] = useState<Alert[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [acking, setAcking] = useState<Set<number>>(new Set());

  const refetch = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const list = await api.listAlerts(
          { include_acked: includeAcked, limit: 200 },
          signal,
        );
        if (signal?.aborted) return;
        setAlerts(list);
        setError(null);
      } catch (err) {
        if (err instanceof DOMException && err.name === "AbortError") return;
        setError(
          err instanceof ApiError
            ? err.message
            : "Couldn't load alerts. Check that Commander is running.",
        );
      }
    },
    [includeAcked],
  );

  // The filter-driven fetch is abortable so a slow older response can't overwrite
  // a newer one when the toggle changes quickly.
  useEffect(() => {
    const ctrl = new AbortController();
    void refetch(ctrl.signal);
    return () => ctrl.abort();
  }, [refetch]);

  // Live: a new alert, a changed unacked count, or a reconnect all trigger a refetch.
  useEventSubscription("alert.raised", () => void refetch());
  useEventSubscription("reconnect", () => void refetch());
  const unacked = snapshot?.unacked_alerts ?? 0;
  useEffect(() => {
    const ctrl = new AbortController();
    void refetch(ctrl.signal);
    return () => ctrl.abort();
  }, [unacked, refetch]);

  const ack = useCallback(
    async (id: number) => {
      setAcking((prev) => new Set(prev).add(id));
      try {
        await api.ackAlert(id);
        toast.success("Alert acknowledged.");
        await refetch();
      } catch (err) {
        toast.error(
          err instanceof ApiError
            ? err.message
            : "Couldn't acknowledge that alert. Try again.",
        );
      } finally {
        setAcking((prev) => {
          const next = new Set(prev);
          next.delete(id);
          return next;
        });
      }
    },
    [refetch, toast],
  );

  return (
    <>
      <header className="surface-head">
        <div>
          <h1 className="surface-head__title">Alerts</h1>
          <p className="surface-head__desc">
            Problems Commander has raised, newest first. Acknowledge one once
            you've dealt with it to clear it from the unread count.
          </p>
        </div>
      </header>

      <div className="toolbar">
        <Toggle
          checked={includeAcked}
          onChange={setIncludeAcked}
          label="Show acknowledged"
        />
      </div>

      {error && (
        <Card pad>
          <p className="notice tone-problem" role="alert">
            <Icon name="problem" size={18} />
            <span>{error}</span>
          </p>
        </Card>
      )}

      {alerts == null ? (
        // Until the first successful load resolves, show a spinner — unless a fetch
        // error is already surfaced above, in which case that notice stands alone.
        error ? null : (
          <div style={{ display: "grid", placeItems: "center", minHeight: "30vh" }}>
            <Spinner label="Loading alerts…" />
          </div>
        )
      ) : alerts.length === 0 ? (
        <Card pad>
          <EmptyState
            icon="safe"
            title={includeAcked ? "No alerts" : "Nothing needs attention"}
            description={
              includeAcked
                ? "Commander hasn't raised any alerts."
                : "There are no unacknowledged problems right now. Turn on “Show acknowledged” to see the history."
            }
          />
        </Card>
      ) : (
        <ul className="alert-list list-reset">
          {alerts.map((a) => (
            <li key={a.id}>
              <AlertRow alert={a} busy={acking.has(a.id)} onAck={() => ack(a.id)} />
            </li>
          ))}
        </ul>
      )}
    </>
  );
}

function AlertRow({
  alert,
  busy,
  onAck,
}: {
  alert: Alert;
  busy: boolean;
  onAck: () => void;
}) {
  const sev = severityView(alert.severity);
  const acked = alert.acked_at !== 0;
  return (
    <Card
      as="article"
      className={`alert-item tone-${sev.tone}${acked ? " alert-item--acked" : ""}`}
      aria-label={`${sev.label} alert: ${alertCategoryLabel(alert.category)}`}
    >
      <div className="alert-item__top">
        <div className="alert-item__tags">
          <StatusPill view={sev} />
          <span className="tag">{alertCategoryLabel(alert.category)}</span>
          {alert.rig_id && (
            <span className="tag">
              <Icon name="rig" size={13} />
              <span className="mono">{alert.rig_id}</span>
            </span>
          )}
        </div>
        {acked ? (
          <StatusPill tone="neutral" icon="safe" label="Acknowledged" />
        ) : (
          <Button
            size="sm"
            variant="secondary"
            loading={busy}
            icon={<Icon name="safe" size={16} />}
            onClick={onAck}
          >
            Acknowledge
          </Button>
        )}
      </div>

      <p className="alert-item__message">{alert.message}</p>

      <div className="alert-item__meta">
        <span>
          Raised{" "}
          <span className="mono" title={absoluteTime(alert.created_at)}>
            {relativeTime(alert.created_at)}
          </span>
        </span>
        {acked && (
          <span>
            Acknowledged{" "}
            <span className="mono" title={absoluteTime(alert.acked_at)}>
              {relativeTime(alert.acked_at)}
            </span>
          </span>
        )}
      </div>
    </Card>
  );
}
