/**
 * Domain-status → presentation mapping. Turns the backend's enums into the
 * {tone, icon, label} triples the UI renders. Centralizing this guarantees the
 * "color + icon + text, never color alone" rule holds everywhere: a status is
 * ALWAYS a tone class (color), an icon, and a text label together.
 */
import type { IconName } from "./icons";
import type {
  FrameState,
  RequestedState,
  RetargetStatus,
  RigState,
  SafeToPowerOff,
  Severity,
} from "../api/types";

/** Semantic tone. Drives the `.tone-*` CSS class that sets the color variables. */
export type Tone = "safe" | "working" | "problem" | "neutral" | "info";

export const toneClass = (tone: Tone): string => `tone-${tone}`;

export interface StatusView {
  tone: Tone;
  icon: IconName;
  label: string;
  /** Plain-language explanation for a tooltip/help line. */
  help?: string;
}

/**
 * Per-reason help copy for an "unknown" verdict. The backend's `power_off_reason`
 * says which of the four distinct conditions produced Unknown, so the badge can be
 * truthful and actionable instead of always claiming "offline or stale". The
 * `missing_on_device` copy deliberately does NOT repeat the `missing_warning`
 * string — the card renders that on its own line — and points at it instead.
 */
function unknownPowerOffHelp(reason?: string): string {
  switch (reason) {
    case "paused":
      return "This rig is paused, so its last check may be incomplete. Resume to refresh, and treat as not safe until then.";
    case "offline":
      return "The rig is offline, so we can't confirm every frame is saved. Treat as not safe.";
    case "offline_during_run":
      return "The rig went offline while a Run now was clearing its backlog, so we can't confirm every frame from that run is saved. Treat as not safe.";
    case "identity":
      return "A different device answered at this address — we can't confirm this rig's frames are saved. Treat as not safe.";
    case "missing_on_device":
      return "Some frames are saved only on the device — treat as not safe. See the warning below.";
    case "removable_absent":
      return "A removable share (USB/microSD) has no medium inserted. Insert it, or turn that share off in the rig's settings, to read safe.";
    case "listing_failed":
      return "The last check didn't complete, so we can't confirm it's safe yet. This usually clears on the next check.";
    case "remote":
      return "The remote this rig is reached through isn't connected, so we can't check the rig. Treat as not safe.";
    default:
      return "The rig is offline or its last listing is stale, so we can't confirm it's safe. Treat as not safe.";
  }
}

/** The hero signal. Rendered EXACTLY as computed — never upgraded to "safe". */
export function safeToPowerOffView(
  s: SafeToPowerOff,
  reason?: string,
): StatusView {
  switch (s) {
    case "safe":
      return {
        tone: "safe",
        icon: "safe",
        label: "Safe to power off",
        help: "Every frame has been copied to the NAS and verified. Powering off now loses nothing.",
      };
    case "working":
      return {
        tone: "working",
        icon: "working",
        label: "Working — don't power off",
        help: "Files are still being copied or checked. Wait until this reads safe before shutting down.",
      };
    case "unknown":
    default:
      return {
        tone: "neutral",
        icon: "unknown",
        // Every other reason keeps the generic label and varies only the help
        // copy; this one names the cause directly, as the design settled it,
        // because "don't rely on this" would otherwise read as a stale check
        // when the real story is a run that got cut off mid-drain.
        label:
          reason === "offline_during_run"
            ? "Unknown — the rig went offline during the last run"
            : "Unknown — don't rely on this",
        help: unknownPowerOffHelp(reason),
      };
  }
}

function baseStateView(state: RigState): StatusView {
  switch (state) {
    case "idle":
      return { tone: "neutral", icon: "clock", label: "Monitoring" };
    case "pulling":
      return { tone: "working", icon: "working", label: "Transferring" };
    case "deleting":
      return { tone: "working", icon: "working", label: "Cleaning up" };
    case "paused":
      return { tone: "neutral", icon: "pause", label: "Paused" };
    case "offline":
      return { tone: "neutral", icon: "unknown", label: "Offline" };
    case "error":
      return { tone: "problem", icon: "problem", label: "Error" };
    default:
      return { tone: "neutral", icon: "unknown", label: state };
  }
}

/**
 * The pending note that follows the pill's label ("Cleaning up · pausing") while
 * the operator's last request (`requested`, recorded by the hub the instant a
 * command is queued) differs from what the worker has actually published. `null`
 * once they agree, so the note disappears. An offline or error rig never gets a
 * note — those states are not something a pending command is racing against, and
 * the table in design frame-lifecycle §12.6 does not call for one there.
 */
function pendingNote(
  state: RigState,
  requested: RequestedState,
  runUntilIdle: boolean,
): string | null {
  if (state === "offline" || state === "error") return null;
  if (runUntilIdle) {
    if (requested === "paused") return "stopping";
    if (requested === "running") return "resuming";
    return null; // requested run_until_idle: agree
  }
  if (state === "paused") {
    if (requested === "running") return "resuming";
    if (requested === "run_until_idle") return "starting run";
    return null; // requested paused: agree
  }
  if (requested === "paused") return "pausing";
  return null; // requested running (or, transiently, run_until_idle): agree
}

/** Whether `rigStateView` would append a pending note for these fields — shared
 * with `showMonitoringPill` so the countdown ring and the pill's note never
 * disagree about whether a command is in flight. */
export function hasPendingNote(
  state: RigState,
  requested: RequestedState,
  runUntilIdle = false,
): boolean {
  return pendingNote(state, requested, runUntilIdle) !== null;
}

export function rigStateView(
  state: RigState,
  requested: RequestedState,
  runUntilIdle = false,
): StatusView {
  // The run label takes precedence over the state-derived one: `State` follows
  // the cycle (idle/pulling/deleting) for the run's duration, but the pill says
  // "Running until idle" throughout, exactly as long as the flag is set.
  const base = runUntilIdle
    ? { tone: "working" as Tone, icon: "working" as IconName, label: "Running until idle" }
    : baseStateView(state);
  const note = pendingNote(state, requested, runUntilIdle);
  return note ? { ...base, label: `${base.label} · ${note}` } : base;
}

/**
 * The "next check in Xs" ring only makes sense on a genuinely idle, unattended
 * rig: hidden behind a pending note (a command is racing the worker), a run in
 * progress (the pill reads "Running until idle" instead), or a draining backlog
 * (the next cycle is already due — design frame-lifecycle §12.6 "Draining
 * indication"). In every one of those cases the plain state pill keeps showing
 * what the listing published, so nothing goes silent.
 */
export function showMonitoringPill(
  state: RigState,
  requested: RequestedState,
  runUntilIdle = false,
  draining = false,
): boolean {
  return (
    state === "idle" &&
    !runUntilIdle &&
    !draining &&
    !hasPendingNote(state, requested, runUntilIdle)
  );
}

/**
 * A frame's lifecycle state as a friendly, color+icon+text status. Every label
 * that follows a verified copy leads with "Archived", so a `deleted` frame (the
 * device original removed after its NAS copy was verified) never reads as "my
 * photo is gone". A `verified` frame is copied and checked but still on the
 * device (kept until its grace elapses): "Archived · on device"; `deleted` is the
 * cleared end state: "Archived · removed from device".
 *
 * When `onHold` is set, a still-on-device verified frame belongs to a rig with
 * keep-originals turned on, so it will never be deleted while that stays on; it
 * reads as "Archived · kept on device" instead.
 */
export function frameStateView(
  state: FrameState,
  opts?: { onHold?: boolean },
): StatusView {
  switch (state) {
    case "verified":
      if (opts?.onHold) {
        return {
          tone: "neutral",
          icon: "clock",
          label: "Archived · kept on device",
          help: "Copied and verified on the NAS. This rig keeps originals, so the device copy is held and won't be deleted until you turn that off.",
        };
      }
      return {
        tone: "working",
        icon: "clock",
        label: "Archived · on device",
        help: "Copied and verified on the NAS. The original stays on the device until its grace period elapses, then it's deleted.",
      };
    case "deleted":
      return {
        tone: "safe",
        icon: "safe",
        label: "Archived · removed from device",
        help: "Copied and verified on the NAS, then the original was removed from the device. Your archived copy is safe.",
      };
    case "unclassified":
      return {
        tone: "neutral",
        icon: "unknown",
        label: "Unclassified",
        help: "The frame's type didn't match a known value, so it's filed to quarantine and never deleted.",
      };
    case "held":
      return {
        tone: "problem",
        icon: "alert",
        label: "Held",
        help: "An integrity check failed at delete time, so the original is kept and you're alerted.",
      };
    case "failed":
      return {
        tone: "problem",
        icon: "problem",
        label: "Failed",
        help: "The transfer or verification failed; Commander retries it with backoff.",
      };
    case "dismissed":
      return {
        tone: "neutral",
        icon: "unknown",
        label: "Dismissed",
        help: "You discarded this permanently-stuck frame, so it no longer counts toward the backlog. Its quarantine copy on the NAS is removed unless another frame still shares it.",
      };
    default:
      return { tone: "neutral", icon: "unknown", label: state };
  }
}

/**
 * A frame whose filed copy went missing from the NAS out of band. `restorePending`
 * is set once a re-pull has been requested (restore_requested_at), reading as
 * "Restore requested" until the original is pulled again. `acked` is set once the
 * operator has confirmed the cull was deliberate (missing_acked_at); it cannot
 * coexist with restorePending (design library-reconciliation §10.4 — Acknowledge
 * refuses a pending restore), but restorePending still takes precedence when the
 * function is asked to choose.
 */
export function missingFrameView(opts?: {
  restorePending?: boolean;
  acked?: boolean;
}): StatusView {
  if (opts?.restorePending) {
    return {
      tone: "working",
      icon: "clock",
      label: "Restore requested",
      help: "A re-pull of the original is queued to replace the copy that went missing from the NAS.",
    };
  }
  if (opts?.acked) {
    // The "warn" tone of design library-reconciliation §4.7 has no literal
    // counterpart in this codebase's Tone union
    // (safe/working/problem/neutral/info) — severityView already maps severity
    // "warn" to tone "working", so this follows that same convention.
    return {
      tone: "working",
      icon: "missing",
      label: "Acknowledged",
      help: "You confirmed this was culled on purpose — the device original is removed on the next pass.",
    };
  }
  return {
    tone: "problem",
    icon: "missing",
    label: "Missing",
    help: "This frame's copy was removed from the NAS outside Commander. Restore it to pull the original from the device again.",
  };
}

/**
 * A rig's missing-frame warning: safe-to-power-off was downgraded to "unknown"
 * because library files are gone while a live source could still supply them. The
 * backend's `missing_warning` string is the explanation shown alongside.
 */
export function missingWarningView(warning: string): StatusView {
  return { tone: "problem", icon: "missing", label: "Frames missing", help: warning };
}

/** One frame's outcome from a retarget apply run (Tools page, model.RetargetResult). */
export function retargetResultView(status: RetargetStatus): StatusView {
  switch (status) {
    case "retargeted":
      return { tone: "safe", icon: "safe", label: "Retargeted" };
    case "skipped":
      return { tone: "neutral", icon: "clock", label: "Skipped" };
    case "failed":
      return { tone: "problem", icon: "problem", label: "Failed" };
    default:
      return { tone: "neutral", icon: "unknown", label: status };
  }
}

export function severityView(sev: Severity): StatusView {
  switch (sev) {
    case "critical":
      return { tone: "problem", icon: "problem", label: "Critical" };
    case "warn":
      return { tone: "working", icon: "alert", label: "Warning" };
    case "info":
    default:
      return { tone: "info", icon: "info", label: "Info" };
  }
}
