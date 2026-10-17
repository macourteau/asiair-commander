/**
 * FrameDetailModal — the shared "everything we recorded about this file" modal,
 * built on the design Dialog. Consumed by both the History surface and the Devices
 * Recent-transfers table. It presents the persisted fields only (raw FITS header
 * cards are never stored), grouped into Identity / Files / Recorded metadata /
 * Timeline sections. Rows whose value is absent or a zero-meaningless sentinel are
 * omitted so the modal stays scannable.
 */
import { useState, type ReactNode } from "react";
import { Dialog, StatusPill, frameStateView } from "@/design";
import { frameHasImage, frameImageUrl } from "@/api";
import type { Astrometry, Frame } from "@/api";
import {
  absoluteTime,
  basename,
  formatArcsecPerPx,
  formatBytes,
  formatDec,
  formatDegrees,
  formatRA,
} from "../util/format";
import { FrameLightbox } from "./FrameLightbox";

export function FrameDetailModal({
  frame,
  rigLabel,
  onHold,
  onClose,
}: {
  frame: Frame | null;
  /** Friendly device name for frame.rig_id; falls back to the id when absent. */
  rigLabel?: string;
  /** True when this verified frame is held by a keep-originals rig (no deletion). */
  onHold?: boolean;
  onClose: () => void;
}) {
  const [zoom, setZoom] = useState(false);
  if (!frame) return null;

  const device = rigLabel || frame.rig_id;
  const sidecars = frame.sidecars ?? [];
  const hasImage = frameHasImage(frame);

  return (
    <Dialog open onClose={onClose} title={basename(frame.source_path) || frame.source_path}>
      <div className="fdetail">
        {hasImage && (
          <button
            type="button"
            className="fdetail-preview"
            onClick={() => setZoom(true)}
            aria-label="Open full-size preview"
          >
            <img loading="lazy" src={frameImageUrl(frame, "card")} alt="" />
          </button>
        )}
        <Section legend="Identity">
          <Row label="Device">{device}</Row>
          <Row label="State">
            <StatusPill view={frameStateView(frame.state, { onHold })} />
          </Row>
          {frame.frame_type && <Row label="Type" mono>{frame.frame_type}</Row>}
          {frame.target && <Row label="Target">{frame.target}</Row>}
          {frame.filter && <Row label="Filter">{frame.filter}</Row>}
          {frame.night > 0 && <Row label="Night">{frame.night}</Row>}
        </Section>

        <Section legend="Files">
          <Row label="Source share" mono>{frame.source_share || "—"}</Row>
          <Row label="Source path" mono>{frame.source_path}</Row>
          {frame.dest_path && <Row label="Destination" mono>{frame.dest_path}</Row>}
          {frame.size > 0 && <Row label="Size">{formatBytes(frame.size)}</Row>}
          {frame.sha256 && <Row label="SHA-256" mono>{frame.sha256}</Row>}
          {sidecars.length > 0 && (
            <Row label="Sidecars" mono>
              {sidecars.map((s) => s.path).join(", ")}
            </Row>
          )}
        </Section>

        {(frame.exptime > 0 ||
          frame.set_temp !== 0 ||
          frame.gain > 0 ||
          frame.offset > 0 ||
          frame.binning ||
          frame.sensor ||
          frame.date_obs) && (
          <Section legend="Recorded metadata">
            {frame.exptime > 0 && <Row label="Exposure">{frame.exptime}s</Row>}
            {frame.set_temp !== 0 && <Row label="Set temp">{frame.set_temp}&deg;C</Row>}
            {frame.gain > 0 && <Row label="Gain">{frame.gain}</Row>}
            {frame.offset > 0 && <Row label="Offset">{frame.offset}</Row>}
            {frame.binning && <Row label="Binning">{frame.binning}</Row>}
            {frame.sensor && <Row label="Sensor">{frame.sensor}</Row>}
            {frame.date_obs && <Row label="Date observed" mono>{frame.date_obs}</Row>}
          </Section>
        )}

        <AstrometrySection astrometry={frame.astrometry} />

        <Section legend="Timeline">
          {frame.mtime > 0 && <Row label="Modified">{absoluteTime(frame.mtime)}</Row>}
          {!!frame.pulled_at && <Row label="Transferred">{absoluteTime(frame.pulled_at)}</Row>}
          {!!frame.deleted_at && <Row label="Deleted">{absoluteTime(frame.deleted_at)}</Row>}
          {frame.attempts > 0 && <Row label="Attempts">{frame.attempts}</Row>}
          {!!frame.next_retry_at && (
            <Row label="Next retry">{absoluteTime(frame.next_retry_at)}</Row>
          )}
          {frame.error && (
            <Row label="Error">
              <span className="tone-problem" style={{ color: "var(--problem-fg)" }}>
                {frame.error}
              </span>
            </Row>
          )}
        </Section>
      </div>
      {zoom && <FrameLightbox frame={frame} onClose={() => setZoom(false)} />}
    </Dialog>
  );
}

/** The human label for a frame's astrometry state, always legible. */
function astroStatusLabel(astrometry?: Astrometry): string {
  if (!astrometry) return "Pending"; // legacy frame the backfill has not reached
  switch (astrometry.status) {
    case "header":
      return "From header";
    case "none":
      return "No solution";
    case "solving":
      return "Solving…";
    case "solved":
      return "Solved";
    case "failed":
      return "Solve failed";
    case "unsolvable":
      return "Cannot solve";
    default:
      return astrometry.status;
  }
}

/**
 * Astrometry section: the WCS solution (center, rotation, scale, FOV, flip) when
 * present, plus the raw solve hints, and always a status label so the state is
 * legible even for a pending or no-solution frame.
 */
function AstrometrySection({ astrometry }: { astrometry?: Astrometry }) {
  const sol = astrometry?.solution;
  const hints = astrometry?.hints;
  return (
    <Section legend="Astrometry">
      <Row label="Status">{astroStatusLabel(astrometry)}</Row>
      {astrometry?.reason && <Row label="Reason">{astrometry.reason}</Row>}
      {sol && (
        <>
          <Row label="Center RA" mono>
            {formatRA(sol.center_ra)}
          </Row>
          <Row label="Center Dec" mono>
            {formatDec(sol.center_dec)}
          </Row>
          <Row label="Rotation" mono>
            {formatDegrees(sol.pos_angle, 2)}
            {sol.flipped ? " (mirror flipped)" : ""}
          </Row>
          <Row label="Pixel scale" mono>
            {formatArcsecPerPx(sol.pixel_scale)}
          </Row>
          {sol.fov_w > 0 && sol.fov_h > 0 && (
            <Row label="Field of view" mono>
              {formatDegrees(sol.fov_w, 2)} × {formatDegrees(sol.fov_h, 2)}
            </Row>
          )}
        </>
      )}
      {hints?.ra !== undefined && (
        <Row label="Object RA" mono>
          {formatRA(hints.ra)}
        </Row>
      )}
      {hints?.dec !== undefined && (
        <Row label="Object Dec" mono>
          {formatDec(hints.dec)}
        </Row>
      )}
      {hints?.focal_len_mm !== undefined && (
        <Row label="Focal length">{hints.focal_len_mm} mm</Row>
      )}
      {hints?.pixel_size_um !== undefined && (
        <Row label="Pixel size">{hints.pixel_size_um} µm</Row>
      )}
    </Section>
  );
}

function Section({ legend, children }: { legend: string; children: ReactNode }) {
  return (
    <section className="fdetail__section">
      <h3 className="fdetail__legend">{legend}</h3>
      <dl className="fdetail__dl">{children}</dl>
    </section>
  );
}

function Row({
  label,
  mono = false,
  children,
}: {
  label: string;
  mono?: boolean;
  children: ReactNode;
}) {
  return (
    <>
      <dt className="fdetail__dt">{label}</dt>
      <dd className={`fdetail__dd${mono ? " mono" : ""}`}>{children}</dd>
    </>
  );
}
