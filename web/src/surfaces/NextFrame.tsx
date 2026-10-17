/**
 * Live "next frame ~in Xs" readout for a rig — when the DEVICE (not Commander) is
 * expected to produce its next light, complementing the Monitoring pill's "when
 * Commander next polls" ring. The interval and ETA are computed on the backend
 * (offset-corrected, capture-time gated); this component only ticks the countdown
 * once a second, mirroring the Monitoring pill/Uptime, and tears the ticker down on
 * unmount.
 *
 * Renders the muted "Waiting for frames…" hint while the rig is online, idle, and
 * has no live cadence, and nothing at all while offline, paused, or mid-copy or
 * mid-cleanup — the pill already carries the first two, and beside a live copy or
 * cleanup readout the hint reads as a stall. A single-frame prediction is a soft guess ("~soon"), never a firm
 * countdown, so it implies no precision it doesn't have.
 */
import { useEffect, useState } from "react";
import type { FramePrediction } from "@/api";
import { formatCountdown } from "../util/format";
import "./next-frame.css";

/** Compact exposure label: 600 → "600s", 1.5 → "1.5s". */
function exposureLabel(sec: number): string {
  return `${Math.round(sec * 100) / 100}s`;
}

export function NextFrame({
  prediction,
  online,
  paused,
  working,
}: {
  prediction?: FramePrediction | null;
  online: boolean;
  paused: boolean;
  /** A copy or cleanup is live (see isWorking). */
  working: boolean;
}) {
  const predicting = prediction?.state === "predicting";
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!predicting) return; // ticker exists only while a live countdown is showing
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, [predicting]);

  // Offline/paused rigs carry their status on the pill; say nothing here.
  if (!online || paused) return null;

  if (!prediction || prediction.state !== "predicting") {
    if (working) return null;
    return <span className="rig-nextframe rig-nextframe--muted">Waiting for frames…</span>;
  }

  const exposure =
    prediction.last_exposure_sec > 0 ? `${exposureLabel(prediction.last_exposure_sec)} · ` : "";

  // Single-frame fallback: interval ≈ exposure, a low-confidence guess rendered
  // softly with no firm countdown.
  if (prediction.sample_count <= 1) {
    return (
      <span className="rig-nextframe rig-nextframe--soft">
        Capturing: <span className="mono">{exposure}next frame ~soon</span>
      </span>
    );
  }

  const remaining = prediction.next_expected_ts - now / 1000;
  const countdown = formatCountdown(remaining);
  const phrase = Math.round(remaining) > 0 ? `next frame ~in ${countdown}` : `next frame ${countdown}`;
  return (
    <span className="rig-nextframe">
      Recent frames: <span className="mono">{exposure}{phrase}</span>
    </span>
  );
}
