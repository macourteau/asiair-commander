/**
 * MonitoringPill — the idle-rig status chip, with the "next check" countdown shown
 * as a small depleting ring in place of the clock glyph rather than as a
 * separate, too-prominent text line. It reuses StatusPill's neutral-tone shell so it
 * lines up with the other status pills; only the leading glyph differs.
 *
 * The ring depletes from full to empty over one poll interval (remaining ÷
 * interval), ticked client-side once a second — a steady "it's alive" signal.
 * The exact seconds live in a real DOM tooltip revealed on hover/focus: an SVG
 * <title> (and the native title attribute) render inconsistently across devices
 * and, once shown, are snapshotted by the browser so they never reflect the ticking value. A plain element rendering React state
 * shows reliably and updates live.
 */
import { useEffect, useState } from "react";
import { toneClass } from "@/design";

const R = 5;
const CIRC = 2 * Math.PI * R;

export function MonitoringPill({
  nextPollAt,
  intervalSec,
}: {
  nextPollAt?: number;
  intervalSec?: number;
}) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, []);

  const remaining = nextPollAt ? nextPollAt - now / 1000 : 0;
  // Deplete from full to empty; without an interval the ring sits full and static.
  const fraction =
    intervalSec && intervalSec > 0
      ? Math.max(0, Math.min(1, remaining / intervalSec))
      : 1;

  const countdown = nextPollAt
    ? remaining > 0
      ? `next check in ${Math.round(remaining)}s`
      : "checking…"
    : "";
  const ariaLabel = countdown ? `Monitoring — ${countdown}` : "Monitoring";

  return (
    <span className="aac-monitoring" aria-label={ariaLabel}>
      <span className={`aac-pill ${toneClass("neutral")}`}>
        <svg
          className="aac-pill__icon"
          width={13}
          height={13}
          viewBox="0 0 14 14"
          fill="none"
          aria-hidden={true}
        >
          <circle
            cx={7}
            cy={7}
            r={R}
            stroke="currentColor"
            strokeOpacity={0.25}
            strokeWidth={2}
          />
          <circle
            cx={7}
            cy={7}
            r={R}
            stroke="currentColor"
            strokeWidth={2}
            strokeLinecap="round"
            strokeDasharray={CIRC}
            strokeDashoffset={CIRC * (1 - fraction)}
            transform="rotate(-90 7 7)"
          />
        </svg>
        <span>Monitoring</span>
      </span>
      {countdown && (
        <span className="aac-monitoring__tip" aria-hidden={true}>
          {countdown}
        </span>
      )}
    </span>
  );
}
