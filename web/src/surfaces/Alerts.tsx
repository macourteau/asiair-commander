/**
 * Alerts — the problem feed. Each alert is a severity (color + icon + TEXT, never
 * color alone), a plain-language category, the rig it belongs to, the message, and
 * when it happened, with an Acknowledge action. Live: refetches on the SSE
 * `alert.raised` event, whenever the snapshot's unacked count changes, and on a
 * stream reconnect (a dropped event self-heals — design architecture §8).
 *
 * `?rig=` narrows the list to one rig (the Dashboard card's "Review" link), with
 * a notice that says so and clears it.
 *
 * After an Acknowledge, focus moves to the next alert's Acknowledge button, or to
 * the page heading when none remain, so a keyboard user is never dropped on the
 * page body when the acknowledged row's button disappears.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router";
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
import {
  ackButtonLabel,
  alertFramesLink,
  alertsForRig,
  nextAckFocus,
  rigDisplayName,
} from "../util/alerts";
import { absoluteTime, relativeTime } from "../util/format";
import "./surfaces.css";

export function Alerts() {
  const snapshot = useSnapshot();
  const toast = useToast();
  const [includeAcked, setIncludeAcked] = useState(false);
  const [alerts, setAlerts] = useState<Alert[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [acking, setAcking] = useState<Set<number>>(new Set());
  const headingRef = useRef<HTMLHeadingElement>(null);
  const listRef = useRef<HTMLUListElement>(null);
  // The alert whose Acknowledge button takes focus once the list re-renders after
  // an ack: a number, null for the heading, or undefined when nothing is pending.
  const [focusAfterAck, setFocusAfterAck] = useState<number | null | undefined>(undefined);
  const rigs = snapshot?.rigs ?? [];
  const [params, setParams] = useSearchParams();
  const rigFilter = params.get("rig") ?? "";
  // Filtered client-side: the list is already capped and fetched whole, so a
  // server-side filter would add an API parameter for no fewer bytes.
  const shown = alerts ? alertsForRig(alerts, rigFilter) : null;

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
      const unackedIds = (shown ?? []).filter((a) => a.acked_at === 0).map((a) => a.id);
      try {
        await api.ackAlert(id);
        toast.success("Alert acknowledged.");
        await refetch();
        setFocusAfterAck(nextAckFocus(unackedIds, id));
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
    [shown, refetch, toast],
  );

  // Runs after the post-ack list has rendered, so the target button exists (or the
  // acknowledged one is gone).
  useEffect(() => {
    if (focusAfterAck === undefined) return;
    const target =
      focusAfterAck === null
        ? null
        : listRef.current?.querySelector<HTMLButtonElement>(`[data-ack-id="${focusAfterAck}"]`);
    (target ?? headingRef.current)?.focus();
    setFocusAfterAck(undefined);
  }, [focusAfterAck, alerts]);

  return (
    <>
      <header className="surface-head">
        <div>
          <h1 className="surface-head__title" ref={headingRef} tabIndex={-1}>
            Alerts
          </h1>
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

      {rigFilter && (
        <div className="notice tone-info" role="status" style={{ marginBottom: "var(--space-3)" }}>
          <Icon name="info" size={18} />
          <span className="grow">Showing only alerts for {rigDisplayName(rigFilter, rigs)}.</span>
          <Button
            size="sm"
            variant="secondary"
            onClick={() =>
              setParams(
                (prev) => {
                  const next = new URLSearchParams(prev);
                  next.delete("rig");
                  return next;
                },
                { replace: true },
              )
            }
          >
            Show all alerts
          </Button>
        </div>
      )}

      {error && (
        <Card pad>
          <p className="notice tone-problem" role="alert">
            <Icon name="problem" size={18} />
            <span>{error}</span>
          </p>
        </Card>
      )}

      {shown == null ? (
        // Until the first successful load resolves, show a spinner — unless a fetch
        // error is already surfaced above, in which case that notice stands alone.
        error ? null : (
          <div style={{ display: "grid", placeItems: "center", minHeight: "30vh" }}>
            <Spinner label="Loading alerts…" />
          </div>
        )
      ) : shown.length === 0 ? (
        <Card pad>
          <EmptyState
            icon="safe"
            title={includeAcked ? "No alerts" : "Nothing needs attention"}
            description={
              includeAcked
                ? `Commander hasn't raised any alerts${rigFilter ? ` for ${rigDisplayName(rigFilter, rigs)}` : ""}.`
                : `There are no unacknowledged problems${rigFilter ? ` on ${rigDisplayName(rigFilter, rigs)}` : ""} right now. Turn on “Show acknowledged” to see the history.`
            }
          />
        </Card>
      ) : (
        <ul className="alert-list list-reset" ref={listRef}>
          {shown.map((a) => (
            <li key={a.id}>
              <AlertRow
                alert={a}
                rigName={a.rig_id ? rigDisplayName(a.rig_id, rigs) : null}
                busy={acking.has(a.id)}
                onAck={() => ack(a.id)}
              />
            </li>
          ))}
        </ul>
      )}
    </>
  );
}

function AlertRow({
  alert,
  rigName,
  busy,
  onAck,
}: {
  alert: Alert;
  /** The rig's display name, or null for a fleet-wide alert. */
  rigName: string | null;
  busy: boolean;
  onAck: () => void;
}) {
  const sev = severityView(alert.severity);
  const acked = alert.acked_at !== 0;
  const category = alertCategoryLabel(alert.category);
  const framesLink = alertFramesLink(alert, rigName ?? "");
  return (
    <Card
      as="article"
      className={`alert-item tone-${sev.tone}${acked ? " alert-item--acked" : ""}`}
      aria-label={`${sev.label} alert: ${category}${rigName ? ` on ${rigName}` : ""}`}
    >
      <div className="alert-item__top">
        <div className="alert-item__tags">
          <StatusPill view={sev} />
          <span className="tag">{category}</span>
          {rigName && (
            <span className="tag">
              <Icon name="rig" size={13} />
              <span>{rigName}</span>
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
            data-ack-id={alert.id}
            aria-label={ackButtonLabel(category, rigName)}
          >
            Acknowledge
          </Button>
        )}
      </div>

      <p className="alert-item__message">{alert.message}</p>
      {framesLink && (
        <p className="alert-item__message">
          <Link to={framesLink.to} aria-label={framesLink.label}>
            {framesLink.text}
          </Link>
        </p>
      )}

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
