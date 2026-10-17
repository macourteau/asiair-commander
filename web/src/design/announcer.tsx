/**
 * Polite aria-live announcer. A single visually-hidden region lives at the app
 * root; any component calls `announce(message)` to have a screen reader read it
 * without moving focus. Announcements are COALESCED — rapid calls (e.g. a burst
 * of SSE events) collapse to the latest within a short window so the SR isn't
 * spammed. Use for status changes only (rig offline, alert raised), never for
 * routine UI chatter.
 */
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";

type Announce = (message: string) => void;

const AnnouncerContext = createContext<Announce | null>(null);

const COALESCE_MS = 500;

export function AnnouncerProvider({ children }: { children: ReactNode }) {
  const [message, setMessage] = useState("");
  const pending = useRef<string[]>([]);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const flush = useCallback(() => {
    timer.current = null;
    const queued = pending.current;
    pending.current = [];
    if (queued.length === 0) return;
    // Collapse a burst to a concise line; join distinct messages politely.
    const text =
      queued.length === 1 ? queued[0]! : `${queued.length} updates. ${queued[queued.length - 1]}`;
    // Clear first so identical consecutive messages still re-announce.
    setMessage("");
    window.setTimeout(() => setMessage(text), 40);
  }, []);

  const announce = useCallback<Announce>(
    (msg) => {
      if (!msg) return;
      pending.current.push(msg);
      if (timer.current == null) {
        timer.current = setTimeout(flush, COALESCE_MS);
      }
    },
    [flush],
  );

  useEffect(
    () => () => {
      if (timer.current != null) clearTimeout(timer.current);
    },
    [],
  );

  return (
    <AnnouncerContext.Provider value={announce}>
      {children}
      <div className="sr-only" role="status" aria-live="polite" aria-atomic="true">
        {message}
      </div>
    </AnnouncerContext.Provider>
  );
}

/** Returns the `announce(message)` function. Safe no-op if no provider is mounted. */
export function useAnnounce(): Announce {
  const ctx = useContext(AnnouncerContext);
  return ctx ?? (() => {});
}
