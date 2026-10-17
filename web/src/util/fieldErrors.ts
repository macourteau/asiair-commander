/**
 * The one shared map from a validation `Problem.Field` to the input key a form
 * surfaces it against, so Settings and every Devices dialog light up the offending
 * field inline instead of dropping it into a top-of-page banner. One map rather
 * than one per surface, because per-surface copies drift apart and silently
 * drop fields.
 *
 * The key convention is surface-agnostic:
 * - A top-level / `defaults.*` Field is its own key, verbatim (`defaults.grace`,
 *   `max_concurrent_transfers`, `templates.light`, …). The Settings input for that
 *   field reads `errs["<that Field>"]`.
 * - A `rigs[<id>].X` Field, when `opts.rigId` matches, maps to the local key `X`
 *   (`grace`, `poll_interval`, …) — exactly what the RigEditor's `<Field error>`
 *   slots key on. A rig problem for a different rig is dropped (it belongs to that
 *   rig's editor); with no `rigId` (Settings) every rig problem is dropped and
 *   stays in the banner.
 * - `web.allowed_hosts` returns the sentinel {@link HOSTS_KEY}, which no per-input
 *   map consumes — it stays routed to AllowedHostsField.
 */
import type { Problem } from "@/api";
import { rigIdFromField } from "./labels";

/** Sentinel key for `web.allowed_hosts`, handled by AllowedHostsField, not an input. */
export const HOSTS_KEY = "__hosts__";

export function fieldErrors(
  problems: Problem[],
  opts?: { rigId?: string },
): Record<string, string> {
  const out: Record<string, string> = {};
  for (const p of problems) {
    const key = inputKey(p.Field, opts?.rigId);
    if (key == null) continue;
    // First problem per field wins, matching the prior per-surface mappers.
    if (out[key] == null) out[key] = p.Message;
  }
  return out;
}

/** The input key a Field maps to, or null when it belongs to no input here. */
function inputKey(field: string, rigId?: string): string | null {
  if (field === "web.allowed_hosts") return HOSTS_KEY;

  const owner = rigIdFromField(field);
  if (owner != null) {
    if (rigId == null || owner !== rigId) return null;
    return field.replace(/^rigs\[[^\]]+\]\.?/, "");
  }
  return field;
}
