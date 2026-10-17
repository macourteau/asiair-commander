/**
 * Feed — the fleet-wide live transfer feed. One row per frame, keyed by
 * rig+share+path, born the moment Commander first sees the file and updated in
 * place through pending → transferring → filed → deleted (design
 * features §4.6). Rows are <div>s in a virtualized list:
 * only visible rows mount, so the fleet can stream thousands of rows cheaply. All
 * merge logic lives in useFrameFeed; this file is presentation + virtualization.
 *
 * Enter/leave animations apply only to rows changing in the visible window from
 * live events (a fresh discovery prepending, a ghost-pending leaving) — not to the
 * mount/unmount churn of scrolling. "New" is tracked by key across the session, so
 * a row scrolled away and back never re-animates.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { api, type Frame, type RigProfile } from "@/api";
import { Card, EmptyState, Icon, IconButton, Spinner } from "@/design";
import { FrameDetailModal } from "./FrameDetailModal";
import { CoordsCell, RotationCell } from "./AstrometryCells";
import { FrameThumb } from "./FrameThumb";
import { FrameLightbox } from "./FrameLightbox";
import { FeedStatus } from "./FeedStatus";
import { useFrameFeed, type FeedRowView } from "./useFrameFeed";
import { absoluteTime, basename, formatBytes, relativeTime } from "../util/format";
import { frameTime } from "../util/frameTime";
import "./surfaces.css";

export function Feed() {
  const { rows, loading, error, loadMore, hasMore } = useFrameFeed();
  const [rigs, setRigs] = useState<RigProfile[]>([]);
  const [selected, setSelected] = useState<Frame | null>(null);
  const [lightbox, setLightbox] = useState<Frame | null>(null);

  useEffect(() => {
    let cancelled = false;
    api
      .listRigs()
      .then((list) => !cancelled && setRigs(list))
      .catch(() => {
        /* the device badge degrades to the raw id if rigs can't load */
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const rigName = useCallback(
    (id: string) => rigs.find((r) => r.id === id)?.name || id,
    [rigs],
  );
  const rigKeepsOriginals = useCallback(
    (id: string) => rigs.find((r) => r.id === id)?.keep_originals ?? false,
    [rigs],
  );

  const scrollRef = useRef<HTMLDivElement>(null);
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => 60,
    overscan: 10,
    getItemKey: (i) => rows[i]?.key ?? i,
  });

  // Rows first seen this session animate on enter; tracked by key so a scroll-
  // remount of an old row never re-animates (design features §4.6).
  const seen = useRef(new Set<string>());
  useEffect(() => {
    for (const r of rows) seen.current.add(r.key);
  });

  // Infinite scroll: when the last virtual row is reached and older pages remain,
  // fetch the next page.
  const items = virtualizer.getVirtualItems();
  useEffect(() => {
    const last = items[items.length - 1];
    if (last && last.index >= rows.length - 1 && hasMore) loadMore();
  }, [items, rows.length, hasMore, loadMore]);

  return (
    <>
      <header className="surface-head">
        <div>
          <h1 className="surface-head__title">Live feed</h1>
          <p className="surface-head__desc">
            Every frame across your rigs as it happens — discovered, copying, filed,
            and cleaned up. Leave this open to watch the whole fleet; scroll back
            through the last 12 hours, or open History for the full record.
          </p>
        </div>
      </header>

      {error ? (
        <Card pad>
          <p className="notice tone-problem" role="alert">
            <Icon name="problem" size={18} />
            <span>{error}</span>
          </p>
        </Card>
      ) : loading && rows.length === 0 ? (
        <div style={{ display: "grid", placeItems: "center", minHeight: "30vh" }}>
          <Spinner label="Loading the feed…" />
        </div>
      ) : rows.length === 0 ? (
        <EmptyState
          icon="nas"
          title="Nothing to show yet"
          description="As frames are captured and copied, they'll stream in here live."
        />
      ) : (
        <Card className="feed-card">
          <div className="feed-head" aria-hidden="true">
            <span />
            <span>Device</span>
            <span>Source</span>
            <span>Type</span>
            <span className="feed-col-right">Size</span>
            <span>Coordinates</span>
            <span className="feed-col-right">Rotation</span>
            <span>Status</span>
            <span className="feed-col-right">When</span>
            <span />
          </div>
          <div ref={scrollRef} className="feed-scroll" role="log" aria-label="Live transfer feed">
            <div style={{ height: virtualizer.getTotalSize(), position: "relative" }}>
              {items.map((vi) => {
                const row = rows[vi.index];
                if (!row) return null;
                const isNew = !seen.current.has(row.key);
                return (
                  <div
                    key={row.key}
                    data-index={vi.index}
                    ref={virtualizer.measureElement}
                    className={`feed-rowwrap${row.removing ? " is-exiting" : ""}${
                      isNew && !virtualizer.isScrolling ? " is-entering" : ""
                    }`}
                    style={{
                      position: "absolute",
                      top: 0,
                      left: 0,
                      width: "100%",
                      transform: `translateY(${vi.start}px)`,
                    }}
                  >
                    <FeedRow
                      row={row}
                      device={rigName(row.rigId)}
                      onHold={rigKeepsOriginals(row.rigId)}
                      onPreview={setLightbox}
                      onDetails={row.frame ? () => setSelected(row.frame!) : undefined}
                    />
                  </div>
                );
              })}
            </div>
          </div>
        </Card>
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

/** One feed row's presentation. Pure: everything it needs is in props. */
export function FeedRow({
  row,
  device,
  onHold,
  onPreview,
  onDetails,
}: {
  row: FeedRowView;
  device: string;
  onHold: boolean;
  onPreview?: (frame: Frame) => void;
  onDetails?: () => void;
}) {
  const name = basename(row.path);
  const when = frameTime(row.frame);
  return (
    <div className="feed-row">
      <FrameThumb frame={row.frame} onOpen={onPreview} />
      <span className="feed-device truncate" title={device}>
        {device}
      </span>
      <span className="feed-source mono truncate" title={row.path}>
        {name}
      </span>
      <span className="feed-type mono">{row.frame?.frame_type || "—"}</span>
      <span className="feed-col-right">{row.size ? formatBytes(row.size) : "—"}</span>
      <span className="feed-coords">
        <CoordsCell astrometry={row.frame?.astrometry} />
      </span>
      <span className="feed-col-right">
        <RotationCell astrometry={row.frame?.astrometry} />
      </span>
      <span className="feed-status">
        <FeedStatus row={row} onHold={onHold} />
      </span>
      <span className="feed-col-right feed-when">
        {when ? (
          <span title={absoluteTime(when)}>{relativeTime(when)}</span>
        ) : (
          ""
        )}
      </span>
      <span className="feed-detail">
        {onDetails && (
          <IconButton label={`Details for ${name}`} icon={<Icon name="info" />} onClick={onDetails} />
        )}
      </span>
    </div>
  );
}
