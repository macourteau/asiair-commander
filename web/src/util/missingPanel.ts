/**
 * The missing panel's pure rules (design library-reconciliation §13.3): each
 * section's table filter, what a folder line offers, and the sentences the panel
 * and its dialogs build from server counts.
 */
import type { AckGroupSkipped, ForgetScope, MissingGroup } from "@/api";

/** One section's table filter. The folder stack ignores it (§13.2). */
export interface SectionFilter {
  /** The folder the table is narrowed to, byte-exact; "" for every folder. */
  dir: string;
  /** Search text matched against the filed path; "" for none. */
  q: string;
  page: number;
}

export const initialSectionFilter: SectionFilter = { dir: "", q: "", page: 1 };

/** Narrow to a folder ("" clears it). A changed filter starts again at page 1,
 * because the old page number means nothing in the new result. */
export function withDir(f: SectionFilter, dir: string): SectionFilter {
  return dir === f.dir ? f : { ...f, dir, page: 1 };
}

/** Change the search text, back to page 1 when it differs. */
export function withQuery(f: SectionFilter, q: string): SectionFilter {
  return q === f.q ? f : { ...f, q, page: 1 };
}

export function pageCount(total: number, pageSize: number): number {
  return pageSize > 0 ? Math.max(1, Math.ceil(total / pageSize)) : 1;
}

/** Step back to the last page when an action emptied the one on screen. Returns
 * `f` itself when nothing changes, so a state update is a no-op. */
export function clampPage(f: SectionFilter, total: number, pageSize: number): SectionFilter {
  const last = pageCount(total, pageSize);
  return f.page > last ? { ...f, page: last } : f;
}

/**
 * The N an actionable folder line's **Acknowledge all N** names, or null when the
 * button does not render: nothing in the folder is acknowledgeable, or the rig
 * keeps its originals, where acknowledging is inert.
 */
export function ackAllCount(g: MissingGroup, keepOriginals: boolean): number | null {
  return keepOriginals || g.ackable <= 0 ? null : g.ackable;
}

const frames = (n: number) => `${n} frame${n === 1 ? "" : "s"}`;

/** Human summary of a group-acknowledge challenge's `skipped` breakdown, or null
 * when it is all zero. */
export function skippedSummary(skipped: AckGroupSkipped): string | null {
  const parts: string[] = [];
  if (skipped.gone > 0) parts.push(`${skipped.gone} already removed from the device`);
  if (skipped.restore_pending > 0) parts.push(`${skipped.restore_pending} with a pending restore`);
  if (skipped.not_verified > 0) parts.push(`${skipped.not_verified} not verified`);
  if (skipped.already > 0) parts.push(`${skipped.already} already acknowledged`);
  return parts.length > 0 ? parts.join(", ") : null;
}

/** The sentence an empty actionable section shows. While the rig's missing warning
 * is up it points at the leftovers instead, without claiming which row the
 * warning means: the warning is a latch that can outlive the row it was raised
 * for (§13.3). */
export function emptyActionableText(missingWarning: boolean): string {
  return missingWarning
    ? "No frames here need action. If the warning is still shown after Commander's next check, the frame it refers to is under Already gone from the device."
    : "No missing frames need action.";
}

/** The label of a leftover-section bulk Forget button. */
export function forgetButtonLabel(scope: ForgetScope, count?: number): string {
  if (scope.older_than_days) return `Forget older than ${scope.older_than_days} days`;
  return count === undefined ? "Forget all" : `Forget all ${count}`;
}

/** What a bulk Forget's dialog says it covers, from the scope it was minted for. */
export function forgetScopeText(scope: ForgetScope): string {
  const where = scope.dir ? "in this folder" : "already gone from the device";
  const age = scope.older_than_days
    ? `, missing for more than ${scope.older_than_days} days`
    : "";
  return `Every frame ${where}${age}.`;
}

/** The bulk Forget dialog's title, naming the server's count. */
export function forgetTitle(count: number): string {
  return `Forget ${frames(count)}?`;
}

/** The Forget confirmation's toast: the count the server actually changed, or the
 * queued notice when the rig's reply timed out. */
export function forgetDoneText(r: { forgotten?: number; code?: string } | undefined): string {
  if (r?.code === "FORGET_QUEUED") return "The rig is busy; this may already have applied.";
  return `Forgot ${frames(r?.forgotten ?? 0)}.`;
}
