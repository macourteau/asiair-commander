/**
 * History — the full combined transfer history across every rig, paginated and
 * filterable by device and/or a date range. Each row opens the shared
 * FrameDetailModal with everything Commander recorded about that file.
 *
 * Live: while the operator is on page 1 (newest-first), a frame.done/frame.failed
 * for any rig refetches the current page through a short trailing debounce, so a
 * left-open History tab fills in as transfers land. On deeper pages the auto-
 * refresh is suppressed — pulling rows out from under a reader is worse than a
 * slightly stale page — and a subtle banner offers a jump back to the live first
 * page; the next filter/page interaction refetches anyway.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import {
  api,
  ApiError,
  useEventSubscription,
  type Frame,
  type RigProfile,
} from "@/api";
import {
  Button,
  Card,
  EmptyState,
  Icon,
  IconButton,
  Select,
  Spinner,
  StatusPill,
  Table,
  frameStateView,
  type Column,
} from "@/design";
import { FrameDetailModal } from "./FrameDetailModal";
import { FrameThumb } from "./FrameThumb";
import { FrameLightbox } from "./FrameLightbox";
import { CoordsCell, RotationCell } from "./AstrometryCells";
import { absoluteTime, basename, formatBytes, relativeTime } from "../util/format";
import "./surfaces.css";

const PAGE_SIZE = 50;

/** A YYYY-MM-DD date input value → unix seconds, or undefined when blank. */
function dayStart(value: string): number | undefined {
  if (!value) return undefined;
  const t = Date.parse(`${value}T00:00:00`);
  return Number.isNaN(t) ? undefined : Math.floor(t / 1000);
}
function dayEnd(value: string): number | undefined {
  if (!value) return undefined;
  const t = Date.parse(`${value}T23:59:59`);
  return Number.isNaN(t) ? undefined : Math.floor(t / 1000);
}

export function History() {
  const [rigs, setRigs] = useState<RigProfile[]>([]);
  const [rig, setRig] = useState("");
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [page, setPage] = useState(1);

  const [frames, setFrames] = useState<Frame[] | null>(null);
  const [total, setTotal] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Frame | null>(null);
  const [lightbox, setLightbox] = useState<Frame | null>(null);
  const [pending, setPending] = useState(false); // live events arrived while off page 1

  useEffect(() => {
    let cancelled = false;
    api
      .listRigs()
      .then((list) => !cancelled && setRigs(list))
      .catch(() => {
        /* the device filter degrades to "All devices" if rigs can't load */
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const rigName = useCallback(
    (id: string) => rigs.find((r) => r.id === id)?.name || id,
    [rigs],
  );

  // Frames span rigs here; look up the owning rig's keep-originals so a held-on-
  // device verified frame reads as "Deletion on hold" rather than "Pending deletion".
  const rigKeepsOriginals = useCallback(
    (id: string) => rigs.find((r) => r.id === id)?.keep_originals ?? false,
    [rigs],
  );

  const load = useCallback(
    async (signal: AbortSignal) => {
      setError(null);
      try {
        const res = await api.listTransferHistory(
          {
            rig: rig || undefined,
            from: dayStart(from),
            to: dayEnd(to),
            page,
            page_size: PAGE_SIZE,
          },
          signal,
        );
        if (signal.aborted) return;
        setFrames(res.frames);
        setTotal(res.total);
        setPending(false);
      } catch (err) {
        if (err instanceof DOMException && err.name === "AbortError") return;
        setError(err instanceof ApiError ? err.message : "Couldn't load the transfer history.");
        setFrames([]);
      }
    },
    [rig, from, to, page],
  );

  // A single in-flight request at a time: issuing a new load aborts the previous
  // one, so a slow older response can't overwrite newer filtered results.
  const inflight = useRef<AbortController | null>(null);
  const debounce = useRef<ReturnType<typeof setTimeout> | null>(null);
  const reload = useCallback(() => {
    inflight.current?.abort();
    const ctrl = new AbortController();
    inflight.current = ctrl;
    void load(ctrl.signal);
  }, [load]);

  useEffect(() => {
    if (debounce.current) clearTimeout(debounce.current); // drop a pending live refetch
    setFrames(null); // spinner on every filter/page change
    reload();
    return () => inflight.current?.abort();
  }, [reload]);

  // Live refresh. A relevant frame event coalesces through a short trailing
  // debounce so a burst becomes one refetch. On page 1 it refetches in place; on a
  // deeper page it only flags that fresher data exists (see the banner below).
  const onFrameEvent = useCallback(() => {
    if (page !== 1) {
      setPending(true);
      return;
    }
    if (debounce.current) clearTimeout(debounce.current);
    debounce.current = setTimeout(reload, 600);
  }, [page, reload]);
  useEventSubscription("frame.done", onFrameEvent);
  useEventSubscription("frame.failed", onFrameEvent);
  useEffect(() => () => {
    if (debounce.current) clearTimeout(debounce.current);
  }, []);

  // Any filter change resets to page 1.
  const onFilter = <T,>(setter: (v: T) => void) => (v: T) => {
    setter(v);
    setPage(1);
  };

  const columns: Column<Frame>[] = [
    {
      key: "thumb",
      header: <span className="sr-only">Preview</span>,
      cell: (f) => <FrameThumb frame={f} onOpen={setLightbox} />,
    },
    { key: "device", header: "Device", cell: (f) => rigName(f.rig_id) },
    {
      key: "file",
      header: "Source file",
      cell: (f) => <span className="mono break-anywhere">{basename(f.source_path)}</span>,
      width: 220,
    },
    { key: "type", header: "Type", cell: (f) => <span className="mono">{f.frame_type || "—"}</span> },
    { key: "size", header: "Size", cell: (f) => formatBytes(f.size), numeric: true },
    {
      key: "coords",
      header: "Coordinates",
      cell: (f) => <CoordsCell astrometry={f.astrometry} />,
    },
    {
      key: "rotation",
      header: "Rotation",
      cell: (f) => <RotationCell astrometry={f.astrometry} />,
      numeric: true,
    },
    {
      key: "state",
      header: "State",
      cell: (f) => (
        <StatusPill
          view={frameStateView(f.state, {
            onHold: f.state === "verified" && rigKeepsOriginals(f.rig_id),
          })}
        />
      ),
    },
    {
      key: "when",
      header: "Transferred",
      cell: (f) => (
        <span title={absoluteTime(f.pulled_at ?? 0)}>{relativeTime(f.pulled_at ?? 0)}</span>
      ),
      numeric: true,
    },
    {
      key: "detail",
      header: <span className="sr-only">Details</span>,
      cell: (f) => (
        <IconButton
          label={`Details for ${basename(f.source_path)}`}
          icon={<Icon name="info" />}
          onClick={() => setSelected(f)}
        />
      ),
    },
  ];

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));
  const rangeStart = total === 0 ? 0 : (page - 1) * PAGE_SIZE + 1;
  const rangeEnd = Math.min(page * PAGE_SIZE, total);

  return (
    <>
      <header className="surface-head">
        <div>
          <h1 className="surface-head__title">History</h1>
          <p className="surface-head__desc">
            Every frame Commander has copied off your rigs, newest first. Filter by
            device or date, and open any row to see everything recorded about it.
          </p>
        </div>
      </header>

      <div className="toolbar">
        <label className="text-sm text-muted" htmlFor="history-rig">
          Device
        </label>
        <div style={{ maxWidth: 240 }}>
          <Select
            id="history-rig"
            value={rig}
            onChange={(e) => onFilter(setRig)(e.target.value)}
            options={[
              { value: "", label: "All devices" },
              ...rigs.map((r) => ({ value: r.id, label: r.name || r.id })),
            ]}
          />
        </div>

        <label className="text-sm text-muted" htmlFor="history-from">
          From
        </label>
        <input
          id="history-from"
          type="date"
          className="aac-input"
          style={{ maxWidth: 170 }}
          value={from}
          max={to || undefined}
          onChange={(e) => onFilter(setFrom)(e.target.value)}
        />
        <label className="text-sm text-muted" htmlFor="history-to">
          To
        </label>
        <input
          id="history-to"
          type="date"
          className="aac-input"
          style={{ maxWidth: 170 }}
          value={to}
          min={from || undefined}
          onChange={(e) => onFilter(setTo)(e.target.value)}
        />
      </div>

      {pending && page !== 1 && (
        <div className="notice tone-info" role="status" style={{ marginBottom: "var(--space-3)" }}>
          <Icon name="info" size={18} />
          <span className="grow">New transfers have arrived since you opened this page.</span>
          <Button size="sm" variant="secondary" onClick={() => setPage(1)}>
            Jump to newest
          </Button>
        </div>
      )}

      {error ? (
        <Card pad>
          <p className="notice tone-problem" role="alert">
            <Icon name="problem" size={18} />
            <span>{error}</span>
          </p>
        </Card>
      ) : !frames ? (
        <div style={{ display: "grid", placeItems: "center", minHeight: "30vh" }}>
          <Spinner label="Loading history…" />
        </div>
      ) : (
        <>
          <Table
            caption="Transfer history"
            columns={columns}
            rows={frames}
            getKey={(f) => f.id}
            empty={
              <EmptyState
                icon="nas"
                title="No transfers match these filters"
                description="Widen the date range or choose a different device to see more."
              />
            }
          />

          {total > 0 && (
            <nav className="pager" aria-label="History pages">
              <Button
                size="sm"
                variant="secondary"
                disabled={page <= 1}
                onClick={() => setPage((p) => Math.max(1, p - 1))}
              >
                Previous
              </Button>
              <span className="pager__status" aria-live="polite">
                {rangeStart}–{rangeEnd} of {total}
              </span>
              <Button
                size="sm"
                variant="secondary"
                disabled={page >= totalPages}
                onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
              >
                Next
              </Button>
            </nav>
          )}
        </>
      )}

      <FrameDetailModal
        frame={selected}
        rigLabel={selected ? rigName(selected.rig_id) : undefined}
        onHold={!!selected && selected.state === "verified" && rigKeepsOriginals(selected.rig_id)}
        onClose={() => setSelected(null)}
      />
      <FrameLightbox frame={lightbox} onClose={() => setLightbox(null)} />
    </>
  );
}
