/**
 * frameTime is the instant a frame row stands for: its transfer time, falling back
 * to the file's mtime when a row was recorded without one. It is the single source
 * for both the feed's "When" column and the order that column appears in, so the
 * list cannot drift out of sequence against the timestamps it renders — a row
 * ordered by anything else (client discovery order, a server id) shows a "4 hours
 * ago" above an "11 minutes ago" whenever the two disagree.
 *
 * 0 for a row with no persisted frame: a live discovery has no timestamp of its own
 * and renders a blank cell.
 */
import type { Frame } from "@/api";

export function frameTime(frame?: Frame | null): number {
  if (!frame) return 0;
  return frame.pulled_at || frame.mtime || 0;
}
