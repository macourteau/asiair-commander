/**
 * Alerts-page helpers: naming an alert's rig, the link from an alert to the
 * frames behind it, the per-rig filter, and where keyboard focus goes after an
 * acknowledgement.
 */
import type { Alert } from "@/api";

/** The ?view= of History that lists a rig's never-deletable frames. */
export const NEVER_DELETABLE_VIEW = "never_deletable";

/** The rig's display name, falling back to its id for a rig no longer configured. */
export function rigDisplayName(rigId: string, rigs: readonly { id: string; name: string }[]): string {
  return rigs.find((r) => r.id === rigId)?.name || rigId;
}

/** The ?show= of Devices that opens a rig's missing-frames panel. */
export const MISSING_PANEL = "missing";
/** The ?show= of Devices that opens a rig's unclassified-frames panel. */
export const UNCLASSIFIED_PANEL = "unclassified";

/** A rig's panel on the Devices page, opened and scrolled to. */
export function devicesPanelLink(rigId: string, panel: string): string {
  return `/devices?${new URLSearchParams({ rig: rigId, show: panel }).toString()}`;
}

/** History filtered to a rig's never-deletable frames: the backlog alert's list. */
export function neverDeletableLink(rigId: string): string {
  return `/history?${new URLSearchParams({ rig: rigId, view: NEVER_DELETABLE_VIEW }).toString()}`;
}

/** The Alerts page filtered to one rig. */
export function rigAlertsLink(rigId: string): string {
  return `/alerts?${new URLSearchParams({ rig: rigId }).toString()}`;
}

/** An alert's "show me" link: where it goes, its text, and its accessible name. */
export interface AlertFramesLink {
  to: string;
  text: string;
  /** Names the rig, so two alerts' links on one page are told apart. */
  label: string;
}

/**
 * The link from an alert to exactly the frames it is about, or null when it has
 * none. The backlog alert goes to History filtered to the rig's never-deletable
 * (unclassified, held or failed) frames; a library_reconciled alert goes to the
 * rig's missing-frames panel in Devices, which lists the frames the scan marked
 * missing, grouped by folder, with the actions that settle them.
 */
export function alertFramesLink(
  alert: Pick<Alert, "category" | "rig_id">,
  rigName: string,
): AlertFramesLink | null {
  if (!alert.rig_id) return null;
  switch (alert.category) {
    case "backlog":
      return {
        to: neverDeletableLink(alert.rig_id),
        text: "Show these frames in History",
        label: `Show ${rigName}'s frames that can't be removed, in History`,
      };
    case "library_reconciled":
      return {
        to: devicesPanelLink(alert.rig_id, MISSING_PANEL),
        text: "Show missing frames in Devices",
        label: `Show ${rigName}'s missing frames, in Devices`,
      };
    default:
      return null;
  }
}

/** The alerts that belong to rigId, or every alert when rigId is empty. */
export function alertsForRig<T extends Pick<Alert, "rig_id">>(alerts: readonly T[], rigId: string): T[] {
  return rigId ? alerts.filter((a) => a.rig_id === rigId) : [...alerts];
}

/** The accessible name of an alert's Acknowledge button, naming its subject. */
export function ackButtonLabel(categoryLabel: string, rigName: string | null): string {
  return rigName ? `Acknowledge: ${categoryLabel} on ${rigName}` : `Acknowledge: ${categoryLabel}`;
}

/**
 * The alert whose Acknowledge button takes focus once `ackedId` is acknowledged:
 * the next unacknowledged one in the list, else the previous one, else null (focus
 * goes to the page heading). `unackedIds` is the list as displayed, before the ack.
 */
export function nextAckFocus(unackedIds: readonly number[], ackedId: number): number | null {
  const i = unackedIds.indexOf(ackedId);
  const rest = unackedIds.filter((id) => id !== ackedId);
  return rest[Math.min(Math.max(i, 0), rest.length - 1)] ?? null;
}
