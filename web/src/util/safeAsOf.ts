import type { SafeToPowerOff } from "@/api";

/**
 * Whether a paused rig's Safe-to-power-off box should show "as of <time>" from
 * `safe_as_of`. Only true on a verdict actually computed from a listing — Yes,
 * Working, or an Unknown a listing itself produced (missing_on_device,
 * removable_absent, listing_failed) — never on the Unknown a *lack* of a fresh
 * listing produced (paused, offline, offline_during_run, identity), and never
 * without a `safe_as_of` to show. An unpaused rig's verdict is always current, so
 * dating it would only add noise (design frame-lifecycle §12.6 "Run now while
 * paused runs until idle" — Safe verdict).
 */
export function showSafeAsOf(
  paused: boolean,
  verdict: SafeToPowerOff,
  reason: string | undefined,
  safeAsOf: number | undefined,
): boolean {
  if (!paused || !safeAsOf) return false;
  if (verdict !== "unknown") return true;
  return (
    reason === "missing_on_device" ||
    reason === "removable_absent" ||
    reason === "listing_failed"
  );
}
