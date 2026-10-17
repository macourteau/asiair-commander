/**
 * Live current-file copy widget: a "Copying now" header with throughput and
 * percent, a progress bar, and the (truncated) file name. Shared by the Dashboard
 * and the Devices rig card so the two readouts can't drift. Renders nothing when
 * no file is copying (current absent or null).
 */
import type { CurrentFile } from "@/api";
import { formatRate } from "../util/format";
import "./transfer-progress.css";

export function TransferProgress({ current }: { current?: CurrentFile | null }) {
  if (!current) return null;
  return (
    <div className="transfer">
      <div className="cluster-between">
        <span className="stat__label">Copying now</span>
        <span className="stat__value">
          {current.bytes_per_sec ? (
            <span className="transfer__rate">{formatRate(current.bytes_per_sec)}</span>
          ) : null}
          {current.pct}%
        </span>
      </div>
      <div
        className="progress"
        role="progressbar"
        aria-valuenow={current.pct}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label={`Copying ${current.file}, ${current.pct}% complete`}
      >
        <div className="progress__bar" style={{ width: `${current.pct}%` }} />
      </div>
      <div className="transfer__file truncate" title={current.file}>
        {current.file}
      </div>
    </div>
  );
}
