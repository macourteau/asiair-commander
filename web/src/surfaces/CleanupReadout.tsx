/**
 * Live delete-pass readout: which frame Commander is checking before removing it
 * from the device, and how far through the pass it is. Each check re-reads the
 * whole device original, so on a slow link a pass can run for many minutes; this
 * keeps it reading as progress rather than a stall. Shares the transfer widget's
 * styles so the two readouts look alike. Renders nothing outside the pass.
 */
import type { CleanupProgress } from "@/api";
import { cleanupPct, cleanupPosition } from "../util/rigActivity";
import "./transfer-progress.css";

export function CleanupReadout({ cleanup }: { cleanup?: CleanupProgress | null }) {
  if (!cleanup) return null;
  const pct = cleanupPct(cleanup);
  const position = cleanupPosition(cleanup);
  return (
    <div className="transfer">
      <div className="cluster-between">
        <span className="stat__label">Checking, then removing from the device</span>
        <span className="stat__value">{position}</span>
      </div>
      <div
        className="progress"
        role="progressbar"
        aria-valuenow={pct}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label={`Cleaning up ${cleanup.file}, frame ${position}`}
      >
        <div className="progress__bar" style={{ width: `${pct}%` }} />
      </div>
      <div className="transfer__file truncate" title={cleanup.file}>
        {cleanup.file}
      </div>
    </div>
  );
}
