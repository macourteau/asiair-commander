/**
 * Alerts-page helpers: naming an alert's rig, the link from an alert to the
 * frames behind it, and where keyboard focus goes after an acknowledgement.
 */
import type { Alert } from "@/api";

/** The ?view= of History that lists a rig's never-deletable frames. */
export const NEVER_DELETABLE_VIEW = "never_deletable";

/** The rig's display name, falling back to its id for a rig no longer configured. */
export function rigDisplayName(rigId: string, rigs: readonly { id: string; name: string }[]): string {
  return rigs.find((r) => r.id === rigId)?.name || rigId;
}

/**
 * Where an alert's "show me" link goes, or null when it has none. The backlog
 * alert links to exactly the frames it is about: History filtered to the rig's
 * never-deletable (unclassified, held or failed) frames.
 */
export function alertFramesLink(alert: Pick<Alert, "category" | "rig_id">): string | null {
  if (alert.category !== "backlog" || !alert.rig_id) return null;
  const q = new URLSearchParams({ rig: alert.rig_id, view: NEVER_DELETABLE_VIEW });
  return `/history?${q.toString()}`;
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
