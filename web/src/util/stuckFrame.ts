/**
 * Plain-language help for a frame that can't be removed from the device
 * automatically (the backlog alert's frames, History `?view=never_deletable`):
 * why it stays, and what clears it. The wording names only actions Commander
 * offers, because the backlog alert promises exactly these (design architecture
 * §10). The raw `frame.error` is kept for a details disclosure; it is written for
 * the logs, not for the operator.
 */
import type { Frame } from "@/api";
import { relativeTime } from "./format";

export interface StuckFrameHelp {
  /** Why the frame stays on the device. */
  why: string;
  /** What clears it. */
  next: string;
  /**
   * Whether the existing Dismiss applies: only an unclassified frame that is not
   * culled (a culled one is leaving through the missing-frames panel, and dismiss
   * would clear the path its deletion needs — design frame-lifecycle §13).
   */
  canDismiss: boolean;
  /**
   * Whether the rig's Run now would try the frame again at once: a failed frame
   * with no retry scheduled ahead, which is one that is due or has used up its
   * attempts (design frame-lifecycle §4.3).
   */
  canRunNow: boolean;
}

/**
 * The accessible name of a row's "Why it stays" button. It starts with the
 * visible text so speech input ("click Why it stays") matches it, then names
 * the frame and rig so each row's button is distinct.
 */
export function whyItStaysLabel(file: string, rig: string): string {
  return `Why it stays: ${file} on ${rig}`;
}

/** The help for a never-deletable frame, or null for a frame in any other state. */
export function stuckFrameHelp(
  frame: Pick<Frame, "state" | "next_retry_at" | "missing_seen_at">,
  now = Date.now(),
): StuckFrameHelp | null {
  switch (frame.state) {
    case "held":
      return {
        why:
          "Just before removing this file from the device, Commander checked it once more and found that " +
          "the device file, the NAS copy and the fingerprint recorded when it was copied no longer all " +
          "match. The file may have changed on the device, or one of the copies may be damaged, so " +
          "Commander keeps both and won't remove the device file.",
        next:
          "There is nothing to dismiss: the device file may be the only good copy. Commander doesn't " +
          "re-check it on its own; if the file on the device changes, Commander copies it again and the " +
          "frame leaves this list. If you have compared the two copies and are sure the NAS copy is good, " +
          "you can delete the device file yourself from the File browser.",
        canDismiss: false,
        canRunNow: false,
      };
    case "failed": {
      const retry = frame.next_retry_at ?? 0;
      const ahead = retry * 1000 > now;
      const when = ahead ? ` The next try is ${relativeTime(retry, now)}.` : "";
      return {
        why: "Commander couldn't copy this frame to the NAS, so the file on the device is the only copy.",
        next:
          "Commander tries again on its own, waiting longer after each failure, up to a set number of " +
          `attempts.${when} Once those run out, Run now on this rig gives it one more try each time you ` +
          "press it. The frame leaves this list once a copy succeeds.",
        canDismiss: false,
        canRunNow: !ahead,
      };
    }
    case "unclassified":
      return {
        why:
          "Commander copied this frame to the NAS but couldn't tell what kind of frame it is from its " +
          "header, so it isn't filed and the device file is kept.",
        next:
          "If its header type only needs an alias, add one in Settings, then reclassify the rig's frames " +
          "in Devices. If the file can never be read, such as a truncated or empty file, dismiss it.",
        canDismiss: !frame.missing_seen_at,
        canRunNow: false,
      };
    default:
      return null;
  }
}
