/**
 * How a rig card reads the server's device breakdown (model.RigStatus.DeviceFrames):
 * what is still on the device and why, one count per reason (design frame-lifecycle
 * §14.5). The server publishes counts; the words are picked here, so the card never
 * claims a frame is waiting out its grace period when nothing will remove it.
 */
import type { DeviceFrames } from "@/api";
import { MISSING_PANEL, devicesPanelLink, neverDeletableLink } from "./alerts";

/** One piece of the sentence; `problem` marks the reason nothing will clear. */
export interface DeviceLinePart {
  text: string;
  problem?: boolean;
}

/** A link from the line to the frames behind one reason. */
export interface DeviceLineLink {
  to: string;
  text: string;
  /** Starts with the visible text and names the rig, so two cards' links differ. */
  label: string;
}

export type DeviceLine =
  /** No listing has completed since startup. */
  | { kind: "unchecked" }
  /** Only uncopied frames remain: the transfer readout and the verdict say so. */
  | { kind: "hidden" }
  | { kind: "drained" }
  | {
      kind: "frames";
      /** The sentence, its parts concatenated. */
      text: string;
      parts: DeviceLinePart[];
      badge: "in_grace" | "keep_originals" | null;
      links: DeviceLineLink[];
    };

const STUCK = "can't be removed automatically";

/** "1 frame" / "3 frames", with the verb that agrees with it. */
function frameCount(n: number): { noun: string; verb: "is" | "are" } {
  return n === 1 ? { noun: "1 frame", verb: "is" } : { noun: `${n} frames`, verb: "are" };
}

export function deviceLine(
  df: DeviceFrames | null | undefined,
  keepOriginals: boolean,
  rigId: string,
  rigName: string,
): DeviceLine {
  if (!df) return { kind: "unchecked" };
  if (df.drained) return { kind: "drained" };

  const links: DeviceLineLink[] = [];
  const stuckLink = () =>
    links.push({
      to: neverDeletableLink(rigId),
      text: "Why they stay",
      label: `Why they stay: ${rigName}'s frames that can't be removed, in History`,
    });

  // A keep-originals rig never deletes, so its line always shows, zero included:
  // there is no drained state to fall back on.
  if (keepOriginals) {
    const kept = frameCount(df.kept);
    const parts: DeviceLinePart[] = [{ text: `${kept.noun} ${kept.verb} kept on the device` }];
    if (df.stuck > 0) {
      parts.push({ text: "; " }, { text: `${df.stuck} ${STUCK}`, problem: true });
      stuckLink();
    }
    return frames(parts, "keep_originals", links);
  }

  const total = df.in_grace + df.due + df.culled + df.stuck;
  if (total === 0) return { kind: "hidden" };
  const badge = df.in_grace > 0 ? "in_grace" : null;
  if (df.stuck > 0) stuckLink();
  if (df.culled > 0) {
    links.push({
      to: devicesPanelLink(rigId, MISSING_PANEL),
      text: "Missing frames",
      label: `Missing frames: ${rigName}'s frames removed from the library, in Devices`,
    });
  }

  const head = `${total} still on the device`;
  const reasons: DeviceLinePart[] = [];
  if (df.in_grace > 0) reasons.push({ text: `${df.in_grace} in grace` });
  if (df.due > 0) reasons.push({ text: `${df.due} due for removal` });
  if (df.culled > 0) reasons.push({ text: `${df.culled} removed from the library` });
  if (df.stuck > 0) reasons.push({ text: `${df.stuck} ${STUCK}`, problem: true });

  // A single reason reads as a plain sentence rather than "N: N …".
  if (reasons.length === 1) {
    const { noun, verb } = frameCount(total);
    if (df.in_grace > 0) return frames([{ text: `${noun} on the device ${verb} in grace` }], badge, links);
    if (df.due > 0) return frames([{ text: `${noun} on the device ${verb} due for removal` }], badge, links);
    if (df.culled > 0) {
      return frames([{ text: `${noun} removed from the library ${verb} leaving the device` }], badge, links);
    }
    return frames([{ text: `${noun} on the device ` }, { text: STUCK, problem: true }], badge, links);
  }
  const parts: DeviceLinePart[] = [{ text: `${head}: ` }];
  reasons.forEach((r, i) => {
    if (i > 0) parts.push({ text: ", " });
    parts.push(r);
  });
  return frames(parts, badge, links);
}

function frames(
  parts: DeviceLinePart[],
  badge: "in_grace" | "keep_originals" | null,
  links: DeviceLineLink[],
): DeviceLine {
  return { kind: "frames", text: parts.map((p) => p.text).join(""), parts, badge, links };
}
