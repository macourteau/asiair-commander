/**
 * useFrameFeed — the live transfer feed's merge engine (design
 * features §4.5). It owns one keyed
 * collection of rows and merges four inputs into it:
 *
 *   - a seed of persisted rows (REST) — the "tail", in the server's id-DESC order;
 *   - each rig's ephemeral `pending[]` (SSE snapshot, gated by `pending_listed`);
 *   - each rig's `current` copying file (SSE snapshot) — the live progress bar;
 *   - the enriched frame.done / frame.failed / frame.deleted events (full Frame).
 *
 * Every input keys on `rig_id + source_share + source_path`, so one row is born
 * "pending" from `pending[]`, becomes "transferring" while it equals its rig's
 * `current`, then "final" when a completion event arrives — all in one slot.
 *
 * Rows are ordered by the timestamp the feed's "When" column renders (`frameTime`):
 * frames in flight, which have no timestamp yet, sit at the top in discovery order;
 * everything with a persisted frame follows in transfer-time order. Ordering by
 * anything else drifts out of sequence against the column, because a completion can
 * report a transfer that finished long before the event arrived — a late plate-solve
 * or a re-file (both keep the original `pulled_at`), a slow transfer that finishes
 * after frames discovered later, or rows carried live across a reconnect that
 * reseeds the server-side rows beneath them.
 *
 * A row's display state is NOT last-writer-wins (the snapshot inputs publish many
 * times per cycle with partial/lagging data). It is derived by strict precedence:
 * a terminal event is sticky and beats any later `current`/`pending` match; else a
 * `current` match renders "transferring"; else "pending". See the state
 * table in design features §4.2. The hook returns rows already annotated with
 * that derived state so the page and the Devices panel render identically off
 * one implementation.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  api,
  useEventSubscription,
  useSnapshot,
  type CurrentFile,
  type Frame,
  type FrameState,
  type PendingFrame,
  type RigStatus,
} from "@/api";
import { frameTime } from "../util/frameTime";

/** Server page size for the fleet seed and each scroll-to-bottom loadMore. */
const FEED_PAGE_SIZE = 200;
/** Newest rows retained across head + tail; older rows live in History. */
const FEED_MAX_ROWS = 2000;
/** The fleet seed window: frames pulled in the trailing 12 hours. */
const SEED_WINDOW_SEC = 12 * 60 * 60;
/** Scoped (Devices) seed size — the newest N across every state. */
const SCOPED_LIMIT = 25;
/** Backstop refetch cadence so state changes the stream can't carry self-heal. */
const FLEET_BACKSTOP_MS = 45_000;
const SCOPED_BACKSTOP_MS = 10_000;
/** How long a leaving row stays mounted so its exit animation can play. */
const LEAVE_MS = 260;

const SEP = "\u0000";
const keyOf = (rigId: string, share: string, path: string) =>
  `${rigId}${SEP}${share}${SEP}${path}`;

/** The internal, mutable row. `zone` marks live rows against server-seeded ones,
 *  which is what reseeding and trimming key off; it does not set render order. */
interface Row {
  key: string;
  rigId: string;
  share: string;
  path: string;
  size: number;
  zone: "head" | "tail";
  /** Set by a completion event (prio 1–2); sticky against non-terminal inputs. */
  terminalState?: FrameState;
  /** The persisted frame, when known (seed row or a completion event). */
  frame?: Frame;
  /** True while the leave animation plays; the row is deleted after it. */
  removing?: boolean;
}

/** The derived display state a row renders in this instant. */
export type FeedDisplay =
  | { kind: "pending" }
  | { kind: "transferring"; current: CurrentFile }
  | { kind: "final"; state: FrameState };

/** A render-facing row: identity + the derived display + any persisted frame. */
export interface FeedRowView {
  key: string;
  rigId: string;
  share: string;
  path: string;
  size: number;
  removing: boolean;
  frame?: Frame;
  display: FeedDisplay;
}

export interface UseFrameFeed {
  rows: FeedRowView[];
  loading: boolean;
  error: string | null;
  /** Fleet mode only: fetch the next older page. No-op in scoped mode. */
  loadMore: () => void;
  hasMore: boolean;
}

/**
 * @param rigId undefined = fleet feed (all rigs, paginated); set = a single rig's
 *   panel (seeded from that rig's newest frames across all states, no scroll-load).
 */
export function useFrameFeed({ rigId }: { rigId?: string } = {}): UseFrameFeed {
  const snapshot = useSnapshot();
  const scoped = rigId !== undefined;

  // The keyed collection. Copy-on-write: every mutation swaps in a fresh Map so
  // React re-renders. Entry order is insertion order (a Map preserves it); the
  // render order is derived from it in `view` below.
  const [rows, setRows] = useState<Map<string, Row>>(() => new Map());
  // A live mirror of `rows` so an effect can test key-presence against the newest
  // collection without re-subscribing when rows change (only on snapshot change).
  const rowsRef = useRef(rows);
  rowsRef.current = rows;
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // Fleet paging over a window frozen at seed time (both ends — see the ordering
  // notes in design features §4.5) so pages join without offset slide.
  const seedFrom = useRef(0);
  const seedTo = useRef(0);
  const page = useRef(1);
  const [total, setTotal] = useState(0);
  // Pages loaded (1 = just the seed). Drives hasMore off page coverage rather than a
  // row tally, so a duplicate/backstop fetch can't corrupt the "more to load" signal.
  const [loadedPages, setLoadedPages] = useState(1);
  // Guards loadMore against overlapping fetches — the infinite-scroll trigger can
  // fire on several renders before a page resolves, which would skip pages.
  const loadingMore = useRef(false);

  // Pending leave-animation finalizers, so unmount/reseed can cancel them.
  const leaveTimers = useRef(new Map<string, ReturnType<typeof setTimeout>>());

  const mutate = useCallback((fn: (draft: Map<string, Row>) => void) => {
    setRows((prev) => {
      const next = new Map(prev);
      fn(next);
      return next;
    });
  }, []);

  // --- leave animation ---------------------------------------------------------

  // Mark a row removing (kept mounted so its exit animation can play), then hard-
  // delete it after the animation. Defined before the reconcile effect that calls
  // it. A ref holds the live implementation so scheduleLeave stays a stable
  // identity (empty-dep useCallback) that never re-triggers the effect.
  const scheduleLeaveRef = useRef<(key: string) => void>(() => {});
  scheduleLeaveRef.current = (key: string) => {
    if (leaveTimers.current.has(key)) return;
    const t = setTimeout(() => {
      leaveTimers.current.delete(key);
      mutate((draft) => {
        // Delete ONLY if the row is still leaving. A completion event or a re-listing
        // can resurrect a d1-marked row (clearing `removing`) between scheduling and
        // firing — the SSE snapshot and event channels are independent, so a terminal
        // event can land just after the authoritative snapshot that marked the row.
        // Deleting unconditionally would drop a just-completed row until the next
        // backstop reseed.
        const r = draft.get(key);
        if (r?.removing) draft.delete(key);
      });
    }, LEAVE_MS);
    leaveTimers.current.set(key, t);
  };
  const scheduleLeave = useCallback((key: string) => scheduleLeaveRef.current(key), []);

  // cancelLeave stops a pending leave and drops its timer, called whenever a row is
  // resurrected so no stale timer lingers.
  const cancelLeaveRef = useRef<(key: string) => void>(() => {});
  cancelLeaveRef.current = (key: string) => {
    const t = leaveTimers.current.get(key);
    if (t !== undefined) {
      clearTimeout(t);
      leaveTimers.current.delete(key);
    }
  };
  const cancelLeave = useCallback((key: string) => cancelLeaveRef.current(key), []);

  useEffect(() => {
    const timers = leaveTimers.current;
    return () => {
      for (const t of timers.values()) clearTimeout(t);
      timers.clear();
    };
  }, []);

  // --- seed / reseed -----------------------------------------------------------

  // Reseed replaces the tail rows (server-order, persisted) while leaving live
  // head rows in place. Used on mount, on reconnect, and by the backstop poll.
  const reseed = useCallback(
    async (signal: AbortSignal, fresh: boolean) => {
      try {
        let seedFrames: Frame[];
        let seedTotal: number;
        if (scoped) {
          const res = await api.listFrames(rigId!, { limit: SCOPED_LIMIT }, signal);
          seedFrames = res.frames;
          seedTotal = res.total;
        } else {
          if (fresh) {
            seedTo.current = Math.floor(Date.now() / 1000);
            seedFrom.current = seedTo.current - SEED_WINDOW_SEC;
            page.current = 1;
          }
          const res = await api.listTransferHistory(
            {
              from: seedFrom.current,
              to: seedTo.current,
              page: 1,
              page_size: FEED_PAGE_SIZE,
            },
            signal,
          );
          seedFrames = res.frames;
          seedTotal = res.total;
        }
        if (signal.aborted) return;
        // A fresh seed (mount/reconnect) and the scoped panel rebuild the whole tail;
        // the fleet backstop instead refreshes page 1 in place so it can't collapse
        // deeper pages the user scrolled to load.
        const replaceTail = scoped || fresh;
        mutate((draft) => {
          if (replaceTail) {
            for (const [k, r] of draft) if (r.zone === "tail") draft.delete(k);
          }
          for (const f of seedFrames) {
            const k = keyOf(f.rig_id, f.source_share, f.source_path);
            const existing = draft.get(k);
            if (existing) {
              // The seed is the newest persisted truth, so it refreshes every row
              // that already reached an outcome — including a live row whose
              // completion event the client missed while the stream was down, which
              // would otherwise hold its pre-completion state until a page reload.
              // A row still in flight is the exception: it has no outcome of its own
              // yet, and the seed may be carrying a PREVIOUS pull's row for the same
              // key (a re-adopted frame, `decide.go:51-55`), which would render the
              // row final while it is still copying.
              if (existing.zone === "tail" || existing.terminalState) {
                existing.terminalState = f.state;
                existing.frame = f;
              }
              continue;
            }
            draft.set(k, {
              key: k,
              rigId: f.rig_id,
              share: f.source_share,
              path: f.source_path,
              size: f.size,
              zone: "tail",
              terminalState: f.state,
              frame: f,
            });
          }
          trim(draft);
        });
        setTotal(seedTotal);
        if (replaceTail && !scoped) {
          page.current = 1;
          setLoadedPages(1);
        }
        setError(null);
      } catch (err) {
        if (err instanceof DOMException && err.name === "AbortError") return;
        setError("Couldn't load the transfer feed.");
      } finally {
        if (!signal.aborted) setLoading(false);
      }
    },
    [scoped, rigId, mutate],
  );

  // Initial seed + backstop poll. Re-runs if the mode/rig changes.
  useEffect(() => {
    setLoading(true);
    const ctrl = new AbortController();
    void reseed(ctrl.signal, true);
    const every = scoped ? SCOPED_BACKSTOP_MS : FLEET_BACKSTOP_MS;
    const timer = setInterval(() => {
      const c = new AbortController();
      void reseed(c.signal, false);
    }, every);
    return () => {
      ctrl.abort();
      clearInterval(timer);
    };
  }, [reseed, scoped]);

  // A reconnect may have missed discrete events; re-seed and let the next
  // authoritative snapshot reconcile pending.
  useEventSubscription("reconnect", () => {
    const c = new AbortController();
    void reseed(c.signal, true);
  });

  // --- snapshot: pending reconcile (only when authoritative) -------------------

  useEffect(() => {
    if (!snapshot) return;
    const relevant = scoped
      ? snapshot.rigs.filter((r) => r.id === rigId)
      : snapshot.rigs;
    const authoritative = relevant.filter((r) => r.pending_listed);
    if (authoritative.length === 0) return;
    mutate((draft) => {
      for (const rig of authoritative) reconcilePending(draft, rig, scheduleLeave, cancelLeave);
    });
  }, [snapshot, scoped, rigId, mutate, scheduleLeave, cancelLeave]);

  // Create a head row for a `current` file we have not seen yet (page opened
  // mid-transfer): the transferring row must exist even before any pending/event.
  // The transferring *state* is derived at render from `current` (deriveDisplay),
  // so this effect only needs to run — and only mutates — when `current` names a
  // key with no row yet. Guarding on that avoids an extra render every ~1s sample.
  useEffect(() => {
    if (!snapshot) return;
    const relevant = scoped
      ? snapshot.rigs.filter((r) => r.id === rigId)
      : snapshot.rigs;
    const fresh = relevant.filter((rig) => {
      const cur = rig.current;
      if (!cur?.source_path || !cur.source_share) return false;
      return !rowsRef.current.has(keyOf(rig.id, cur.source_share, cur.source_path));
    });
    if (fresh.length === 0) return;
    mutate((draft) => {
      for (const rig of fresh) {
        const cur = rig.current!;
        const k = keyOf(rig.id, cur.source_share!, cur.source_path!);
        if (draft.has(k)) continue;
        draft.set(k, {
          key: k,
          rigId: rig.id,
          share: cur.source_share!,
          path: cur.source_path!,
          size: 0,
          zone: "head",
        });
      }
    });
  }, [snapshot, scoped, rigId, mutate]);

  // --- completion events -------------------------------------------------------

  // A completion event carries the frame's own persisted state (frame.done can be
  // verified OR unclassified; failed → failed; deleted → deleted), so terminalState
  // is taken from the frame, never a fixed label. allowCreate is false only for
  // frame.deleted: never prepend a removed-from-device row for a frame the operator never
  // saw arrive (one that aged out of the window).
  const onTerminal = useCallback(
    (allowCreate: boolean) => (data: unknown) => {
      const f = data as Frame;
      if (!f?.source_path || !f.source_share) return;
      if (scoped && f.rig_id !== rigId) return;
      const k = keyOf(f.rig_id, f.source_share, f.source_path);
      cancelLeave(k); // a completion resurrects a d1-marked row; stop its leave timer
      mutate((draft) => {
        const existing = draft.get(k);
        if (existing) {
          // Update in place, keeping the slot. Terminal is sticky against
          // non-terminal inputs; terminal→terminal is last-event-wins.
          existing.terminalState = f.state;
          existing.frame = f;
          existing.removing = false;
        } else if (allowCreate) {
          draft.set(k, {
            key: k,
            rigId: f.rig_id,
            share: f.source_share,
            path: f.source_path,
            size: f.size,
            zone: "head",
            terminalState: f.state,
            frame: f,
          });
          trim(draft);
        }
      });
    },
    [scoped, rigId, mutate, cancelLeave],
  );

  useEventSubscription("frame.done", onTerminal(true));
  useEventSubscription("frame.failed", onTerminal(true));
  useEventSubscription("frame.deleted", onTerminal(false));

  // --- loadMore (fleet only) ---------------------------------------------------

  const loadMore = useCallback(() => {
    if (scoped || loadingMore.current) return; // one page fetch at a time
    loadingMore.current = true;
    const ctrl = new AbortController();
    const nextPage = page.current + 1;
    void api
      .listTransferHistory(
        {
          from: seedFrom.current,
          to: seedTo.current,
          page: nextPage,
          page_size: FEED_PAGE_SIZE,
        },
        ctrl.signal,
      )
      .then((res) => {
        page.current = nextPage;
        setLoadedPages(nextPage);
        mutate((draft) => {
          for (const f of res.frames) {
            const k = keyOf(f.rig_id, f.source_share, f.source_path);
            if (draft.has(k)) continue; // deduped to its existing (head) instance
            draft.set(k, {
              key: k,
              rigId: f.rig_id,
              share: f.source_share,
              path: f.source_path,
              size: f.size,
              zone: "tail",
              terminalState: f.state,
              frame: f,
            });
          }
          trim(draft);
        });
        setTotal(res.total);
      })
      .catch(() => {
        /* a failed page-load leaves what's already shown; the user can retry */
      })
      .finally(() => {
        loadingMore.current = false;
      });
  }, [scoped, mutate]);

  // More to load while the pages fetched so far don't yet cover the window total.
  // Page-coverage, not a row count, so it's immune to dedup and duplicate fetches.
  const hasMore = !scoped && loadedPages * FEED_PAGE_SIZE < total;

  // --- derive the render list --------------------------------------------------

  const view = useMemo<FeedRowView[]>(() => {
    // Live `current` per rig, for the transferring-state derivation.
    const currentByRig = new Map<string, CurrentFile>();
    for (const rig of snapshot?.rigs ?? []) {
      if (rig.current?.source_path) currentByRig.set(rig.id, rig.current);
    }

    // Rows the operator can date — every row carrying a persisted frame — are
    // ordered by the exact timestamp the "When" column renders, so the column can
    // never read out of sequence. Rows still in flight have no such timestamp
    // (nothing has been pulled yet), so they hold the top in discovery order.
    const live: Row[] = [];
    const dated: Row[] = [];
    for (const r of rows.values()) (r.frame ? dated : live).push(r);
    live.reverse(); // newest discovery on top; a Map iterates oldest-first
    // A stable sort, so frames sharing a second keep the order they entered the
    // collection: the server's id DESC within a seeded page, arrival order live.
    dated.sort((a, b) => frameTime(b.frame) - frameTime(a.frame));

    return [...live, ...dated].map((r) => ({
      key: r.key,
      rigId: r.rigId,
      share: r.share,
      path: r.path,
      size: r.size,
      removing: !!r.removing,
      frame: r.frame,
      display: deriveDisplay(r, currentByRig.get(r.rigId)),
    }));
  }, [rows, snapshot]);

  return { rows: view, loading, error, loadMore, hasMore };
}

// --- pure merge helpers --------------------------------------------------------

/**
 * The row's display state, highest-priority condition first (design state table):
 * a terminal event is sticky; else a `current` match is "transferring"; else
 * "pending". A terminal row ignores `current`/`pending` entirely.
 */
function deriveDisplay(r: Row, current: CurrentFile | undefined): FeedDisplay {
  if (r.terminalState) return { kind: "final", state: r.terminalState };
  if (
    current &&
    current.source_share === r.share &&
    current.source_path === r.path
  ) {
    return { kind: "transferring", current };
  }
  return { kind: "pending" };
}

/**
 * Reconcile one rig's authoritative `pending[]` into the collection: upsert each
 * entry as a head row (pending unless already terminal), then d1-remove any head
 * row of this rig still pending (no terminal event) whose key left the set. Only
 * ever called for a snapshot carrying `pending_listed === true`.
 */
function reconcilePending(
  draft: Map<string, Row>,
  rig: RigStatus,
  scheduleLeave: (key: string) => void,
  cancelLeave: (key: string) => void,
) {
  const pending: PendingFrame[] = rig.pending ?? [];
  const present = new Set<string>();
  for (const p of pending) {
    const k = keyOf(rig.id, p.source_share, p.source_path);
    present.add(k);
    const existing = draft.get(k);
    if (existing) {
      if (existing.removing) {
        existing.removing = false; // re-listed → cancel the pending exit
        cancelLeave(k);
      }
      if (existing.size === 0 && p.size) existing.size = p.size;
      continue; // never downgrade a terminal/known row
    }
    draft.set(k, {
      key: k,
      rigId: rig.id,
      share: p.source_share,
      path: p.source_path,
      size: p.size,
      zone: "head",
    });
  }
  // d1: a still-pending head row of this rig absent from the complete set is a
  // ghost (a temp file the ASIAIR removed before it ever transferred).
  for (const r of draft.values()) {
    if (r.rigId !== rig.id || r.zone !== "head") continue;
    if (r.terminalState || r.removing) continue;
    if (!present.has(r.key)) {
      r.removing = true;
      scheduleLeave(r.key);
    }
  }
}

/**
 * Bound the retained rows at FEED_MAX_ROWS, trimming the oldest rows overall:
 * the oldest tail rows first (they are the last-inserted, since pages load
 * newest→oldest), then the oldest head rows only in an extreme long session.
 * Rebuilds the Map so the kept subset keeps its insertion order.
 */
function trim(draft: Map<string, Row>) {
  if (draft.size <= FEED_MAX_ROWS) return;
  const headKeys: string[] = [];
  const tailKeys: string[] = [];
  for (const [k, r] of draft) (r.zone === "head" ? headKeys : tailKeys).push(k);

  let keepHead = headKeys;
  let keepTail = tailKeys;
  if (headKeys.length >= FEED_MAX_ROWS) {
    keepTail = [];
    keepHead = headKeys.slice(headKeys.length - FEED_MAX_ROWS); // newest head
  } else {
    keepTail = tailKeys.slice(0, FEED_MAX_ROWS - headKeys.length); // newest tail
  }

  const kept = new Set([...keepHead, ...keepTail]);
  for (const k of draft.keys()) {
    if (!kept.has(k)) draft.delete(k);
  }
}
