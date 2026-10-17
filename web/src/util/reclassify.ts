/**
 * reclassifyOutcome phrases the toast a finished reclassify pass raises.
 *
 * Reclassify re-files frames that are ALREADY on the NAS, so there is no transfer
 * to watch: nothing new arrives in the live feed, and the re-filed rows keep their
 * original place in it (hours down, if the frames were pulled hours ago). The
 * counts are the only evidence the pass did anything, so they are spelled out
 * rather than left for the operator to infer from a table that quietly emptied.
 *
 * A pass that moved nothing points at the fix, because that is the case where the
 * operator is most likely to conclude the button is broken.
 */
import type { RigReclassifiedEvent } from "@/api";

export function reclassifyOutcome({ examined, moved }: RigReclassifiedEvent): string {
  if (examined === 0) return "Reclassify finished — no frames were being held.";
  const held = `${examined} held frame${examined === 1 ? "" : "s"}`;
  if (moved === 0) {
    return `Reclassify finished — rechecked ${held}, none matched a known type. Add the missing header value to the alias tables in Settings.`;
  }
  if (moved === examined) {
    return `Reclassify finished — re-filed ${moved === 1 ? "the held frame" : `all ${held}`} into the library.`;
  }
  return `Reclassify finished — re-filed ${moved} of ${held}; the rest still need an alias.`;
}
