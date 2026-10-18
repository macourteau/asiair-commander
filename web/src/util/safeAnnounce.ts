/**
 * Screen-reader announcements for a rig's safe-to-power-off verdict crossing
 * into or out of "safe" between two snapshots. The verdict is the dashboard's
 * hero; a listener who cannot see the card is told when it flips, without an
 * aria-live region on every card re-reading each snapshot.
 */
import type { Snapshot } from "@/api";

/**
 * One message per rig present in both snapshots whose verdict entered or left
 * "safe". No previous snapshot (the first one after connecting) yields nothing,
 * so opening the page does not read out the whole fleet.
 */
export function safeTransitions(prev: Snapshot | null, next: Snapshot): string[] {
  if (!prev) return [];
  const before = new Map(prev.rigs.map((r) => [r.id, r.safe_to_power_off === "safe"]));
  const out: string[] = [];
  for (const rig of next.rigs) {
    const was = before.get(rig.id);
    if (was === undefined) continue;
    const now = rig.safe_to_power_off === "safe";
    if (now === was) continue;
    out.push(now ? `${rig.name} is safe to power off.` : `${rig.name} is no longer safe to power off.`);
  }
  return out;
}
