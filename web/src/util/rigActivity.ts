import type { CleanupProgress, RigStatus } from "@/api";

/**
 * Whether the rig is copying or cleaning up right now. The idle "Waiting for
 * frames…" hint must not show then: beside a live copy or cleanup it reads as the
 * rig having stalled. Judged by the live readouts, not by state: the state keeps
 * the last phase's value ("pulling"/"deleting") until the next cycle begins, so
 * it stays set while the rig sits idle between cycles.
 */
export function isWorking(rig: Pick<RigStatus, "current" | "cleanup">): boolean {
  return Boolean(rig.current || rig.cleanup);
}

/** The delete pass's position as a plain phrase, e.g. "12 of 50". */
export function cleanupPosition(c: CleanupProgress): string {
  return `${c.index} of ${c.total}`;
}

/**
 * Percent of the pass behind the frame being checked now: 12 of 50 has finished
 * 11. Clamped, so a malformed position can't overflow the bar.
 */
export function cleanupPct(c: CleanupProgress): number {
  if (c.total <= 0) return 0;
  const pct = Math.round(((c.index - 1) / c.total) * 100);
  return Math.min(100, Math.max(0, pct));
}
