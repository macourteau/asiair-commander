/**
 * Shared table cells for a frame's astrometry, used by both the History and the
 * Devices "Recent transfers" tables so the two stay in lockstep. A frame with no
 * WCS solution (calibration frame, unsolved light, or a legacy frame the backfill
 * has not reached) renders an em dash. Tier 1 in-progress/failed states render a
 * compact status word in place of coordinates.
 */
import type { ReactNode } from "react";
import { Spinner } from "@/design";
import type { Astrometry } from "@/api";
import { formatDec, formatDegrees, formatRA } from "../util/format";

const DASH = <span className="text-muted">—</span>;

/**
 * The Tier 1 status word shown in place of a solution while solving, or after a
 * solve failed. Returns null for every other status so the cell falls through to
 * its normal solution/DASH rendering. Kept compact for dense virtualized rows.
 */
function astroStatusCell(astrometry?: Astrometry): ReactNode | null {
  switch (astrometry?.status) {
    case "solving":
      return <Spinner size={12} label="solving…" />;
    case "unsolvable":
      return (
        <span className="text-muted" title={astrometry.reason}>
          cannot solve
        </span>
      );
    case "failed":
      return (
        <span className="text-muted" title={astrometry.reason}>
          solve failed
        </span>
      );
    default:
      return null;
  }
}

/** Center RA/Dec of the frame, or an em dash when there is no solution. */
export function CoordsCell({ astrometry }: { astrometry?: Astrometry }) {
  const status = astroStatusCell(astrometry);
  if (status) return status;
  const s = astrometry?.solution;
  if (!s) return DASH;
  return (
    <span className="mono astro-coords">
      {formatRA(s.center_ra)} {formatDec(s.center_dec)}
    </span>
  );
}

/** Rotation (position angle) with a small mirror-flip marker, or an em dash. */
export function RotationCell({ astrometry }: { astrometry?: Astrometry }) {
  const status = astroStatusCell(astrometry);
  if (status) return status;
  const s = astrometry?.solution;
  if (!s) return DASH;
  return (
    <span className="mono">
      {formatDegrees(s.pos_angle, 1)}
      {s.flipped && (
        <span className="astro-flip" title="Mirror flipped (det(CD) > 0)">
          {" "}
          ⇋
        </span>
      )}
    </span>
  );
}
