/**
 * SSE layer. A single EventSource on `/events` feeds the whole app:
 *
 *  - `event: snapshot` — the coalesced, latest-wins whole-fleet Snapshot. Stored
 *    and exposed via useSnapshot(); a slow render just sees the newest value.
 *  - discrete events — frame.started/done/failed, rig.online/offline,
 *    alert.raised — fanned out to subscribers registered with subscribe().
 *
 * On any drop we reconnect with exponential backoff + jitter, and on a successful
 * RE-connect we emit a synthetic "reconnect" event so consumers refetch
 * authoritative data (alerts, lists) that discrete events may have missed while
 * we were disconnected (design architecture §8: a lossy stream self-heals via
 * refetch).
 *
 * Significant changes (a rig going offline, a new alert, a rig's safe-to-power-off
 * verdict entering or leaving "safe") are announced politely
 * to screen readers via the coalescing announcer — never spammed.
 */
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useAnnounce } from "../design/announcer";
import { safeTransitions } from "../util/safeAnnounce";
import type {
  AlertRaisedEvent,
  RigPresenceEvent,
  ServerInfo,
  Snapshot,
} from "./types";

export type ConnState = "connecting" | "open" | "reconnecting" | "closed";

/** Wire event names, plus the synthetic "reconnect" signal. */
export type EventName =
  | "snapshot"
  | "frame.started"
  | "frame.done"
  | "frame.failed"
  | "frame.deleted"
  | "rig.online"
  | "rig.offline"
  | "rig.reclassified"
  | "alert.raised"
  | "reconnect";

type Handler = (data: unknown) => void;

interface EventsContextValue {
  /** Latest whole-fleet snapshot, or null until the first one arrives. */
  snapshot: Snapshot | null;
  /** Process constants (version, start time), or null until server_info arrives. */
  serverInfo: ServerInfo | null;
  connState: ConnState;
  /** Subscribe to a wire/synthetic event. Returns an unsubscribe function. */
  subscribe: (event: EventName, handler: Handler) => () => void;
}

const EventsContext = createContext<EventsContextValue | null>(null);

const BACKOFF_MIN = 1000;
const BACKOFF_MAX = 30000;

const DISCRETE_EVENTS: EventName[] = [
  "frame.started",
  "frame.done",
  "frame.failed",
  "frame.deleted",
  "rig.online",
  "rig.offline",
  "rig.reclassified",
  "alert.raised",
];

export function EventsProvider({ children }: { children: ReactNode }) {
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [serverInfo, setServerInfo] = useState<ServerInfo | null>(null);
  const [connState, setConnState] = useState<ConnState>("connecting");
  const announce = useAnnounce();

  const subscribers = useRef(new Map<EventName, Set<Handler>>());
  const esRef = useRef<EventSource | null>(null);
  const backoff = useRef(BACKOFF_MIN);
  const retryTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const hadConnected = useRef(false);
  const closedByUs = useRef(false);
  const lastSnapshot = useRef<Snapshot | null>(null);

  const emit = useCallback((event: EventName, data: unknown) => {
    const set = subscribers.current.get(event);
    if (!set) return;
    for (const h of set) {
      try {
        h(data);
      } catch (err) {
        console.error(`events: subscriber for ${event} threw`, err);
      }
    }
  }, []);

  const subscribe = useCallback<EventsContextValue["subscribe"]>(
    (event, handler) => {
      let set = subscribers.current.get(event);
      if (!set) {
        set = new Set();
        subscribers.current.set(event, set);
      }
      set.add(handler);
      return () => set!.delete(handler);
    },
    [],
  );

  useEffect(() => {
    closedByUs.current = false;

    const connect = () => {
      const es = new EventSource("/events", { withCredentials: true });
      esRef.current = es;

      es.addEventListener("open", () => {
        backoff.current = BACKOFF_MIN;
        setConnState("open");
        if (hadConnected.current) {
          // A reconnect: tell consumers to refetch what the stream may have missed.
          emit("reconnect", undefined);
        }
        hadConnected.current = true;
      });

      es.addEventListener("snapshot", (e) => {
        try {
          const next = JSON.parse((e as MessageEvent).data) as Snapshot;
          for (const msg of safeTransitions(lastSnapshot.current, next)) announce(msg);
          lastSnapshot.current = next;
          setSnapshot(next);
        } catch (err) {
          console.error("events: bad snapshot payload", err);
        }
      });

      es.addEventListener("server_info", (e) => {
        try {
          setServerInfo(JSON.parse((e as MessageEvent).data) as ServerInfo);
        } catch (err) {
          console.error("events: bad server_info payload", err);
        }
      });

      for (const name of DISCRETE_EVENTS) {
        es.addEventListener(name, (e) => {
          let data: unknown;
          try {
            data = JSON.parse((e as MessageEvent).data);
          } catch {
            data = undefined;
          }
          announceDiscrete(name, data, announce);
          emit(name, data);
        });
      }

      es.addEventListener("error", () => {
        // EventSource will try its own reconnect; we take control to add backoff
        // and a refetch signal, so close and schedule our own retry.
        es.close();
        esRef.current = null;
        if (closedByUs.current) return;
        setConnState("reconnecting");
        const jitter = Math.random() * 0.3 * backoff.current;
        const delay = Math.min(backoff.current, BACKOFF_MAX) + jitter;
        backoff.current = Math.min(backoff.current * 2, BACKOFF_MAX);
        retryTimer.current = setTimeout(connect, delay);
      });
    };

    connect();

    return () => {
      closedByUs.current = true;
      if (retryTimer.current != null) clearTimeout(retryTimer.current);
      esRef.current?.close();
      esRef.current = null;
      setConnState("closed");
    };
  }, [emit, announce]);

  const value = useMemo<EventsContextValue>(
    () => ({ snapshot, serverInfo, connState, subscribe }),
    [snapshot, serverInfo, connState, subscribe],
  );

  return <EventsContext.Provider value={value}>{children}</EventsContext.Provider>;
}

function announceDiscrete(name: EventName, data: unknown, announce: (m: string) => void) {
  switch (name) {
    case "rig.offline": {
      const d = data as RigPresenceEvent;
      announce(`Rig ${d?.name ?? d?.rig_id ?? ""} went offline.`);
      break;
    }
    case "rig.online": {
      const d = data as RigPresenceEvent;
      announce(`Rig ${d?.name ?? d?.rig_id ?? ""} is back online.`);
      break;
    }
    case "alert.raised": {
      const d = data as AlertRaisedEvent;
      announce(`New ${d?.severity ?? ""} alert: ${d?.message ?? "a problem was raised"}.`);
      break;
    }
    default:
      break; // frame.* are routine; not announced
  }
}

function useEventsContext(): EventsContextValue {
  const ctx = useContext(EventsContext);
  if (!ctx) throw new Error("useEvents must be used within <EventsProvider>");
  return ctx;
}

/** The latest whole-fleet snapshot (null until the first arrives). */
export function useSnapshot(): Snapshot | null {
  return useEventsContext().snapshot;
}

/** The live connection state, for a header status indicator. */
export function useConnState(): ConnState {
  return useEventsContext().connState;
}

/** Process constants (app version, start time), or null until server_info arrives. */
export function useServerInfo(): ServerInfo | null {
  return useEventsContext().serverInfo;
}

/**
 * Subscribe to a discrete event (or "reconnect") for the lifetime of a component.
 * Example: useEventSubscription("alert.raised", () => refetchAlerts()).
 */
export function useEventSubscription(event: EventName, handler: Handler) {
  const { subscribe } = useEventsContext();
  const ref = useRef(handler);
  ref.current = handler;
  useEffect(
    () => subscribe(event, (data) => ref.current(data)),
    [event, subscribe],
  );
}
