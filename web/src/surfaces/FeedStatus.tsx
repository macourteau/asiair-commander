/**
 * FeedStatus — the shared status cell for a live-feed row, rendered identically by
 * the fleet Feed page and the Devices "Recent transfers" panel (design
 * features §4.6). It shows one of three things by the
 * row's derived display state: a dimmed "Pending transfer" label (no DB state, so
 * deliberately not a StatusPill), a live progress bar while copying, or the
 * persisted-state pill once final/deleted.
 */
import { StatusPill, frameStateView } from "@/design";
import type { FeedRowView } from "./useFrameFeed";
import { basename, formatRate } from "../util/format";

export function FeedStatus({ row, onHold }: { row: FeedRowView; onHold: boolean }) {
  switch (row.display.kind) {
    case "pending":
      return <span className="feed-pending">Pending transfer</span>;
    case "transferring": {
      const { pct, bytes_per_sec } = row.display.current;
      return (
        <span className="feed-progress">
          <span
            className="progress feed-progress__bar"
            role="progressbar"
            aria-valuenow={pct}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-label={`Copying ${basename(row.path)}, ${pct}%`}
          >
            <span className="progress__bar" style={{ width: `${pct}%` }} />
          </span>
          <span className="feed-progress__pct">
            {bytes_per_sec ? `${formatRate(bytes_per_sec)} · ` : ""}
            {pct}%
          </span>
        </span>
      );
    }
    case "final":
      return (
        <StatusPill
          view={frameStateView(row.display.state, {
            onHold: row.display.state === "verified" && onHold,
          })}
        />
      );
  }
}
