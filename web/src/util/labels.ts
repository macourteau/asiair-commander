/**
 * Plain-language labels for backend enum keys (design architecture §9.2
 * UI-writing: a non-programmer should understand every label). Kept out of the
 * design layer because these are domain vocabulary, not visual tokens.
 */
import type { AlertCategory, Problem } from "@/api";

const ALERT_CATEGORY_LABELS: Record<string, string> = {
  transfer_failed: "Transfer failed",
  verify_mismatch: "Verify mismatch",
  delete_mismatch: "Delete mismatch",
  unclassified: "Unclassified frame",
  rot_clash: "Rotation clash",
  backlog: "Backlog building up",
  nas_disk_low: "NAS disk low",
  destination_unavailable: "Destination unreadable",
  device_disk_low: "Device disk low",
  unreachable: "Rig unreachable",
  frame_lost: "Frame lost",
  identity_mismatch: "Rig identity mismatch",
  identity_unconfirmed: "Rig identity unconfirmed",
  media_changed: "Removable media changed",
  library_reconciled: "Library reconciled",
  library_unavailable: "Library unavailable",
  library_mismatch: "Library mismatch",
  clock_skew: "Clock skew",
  config: "Configuration problem",
  refile: "Re-file problem",
  remote_unreachable: "Remote unreachable",
  remote_stalled: "No progress through a remote",
  remote_pairing: "Remote pairing in use twice",
  remote_foreign_key: "Remote refused an unknown key",
  remote_build_stale: "Remote not updating",
};

/** Human phrase for an alert category, falling back to a de-slugged key. */
export function alertCategoryLabel(category: AlertCategory): string {
  return ALERT_CATEGORY_LABELS[category] ?? deslug(String(category));
}

/** "device_disk_low" → "Device disk low". */
export function deslug(key: string): string {
  const s = key.replace(/[_-]+/g, " ").trim();
  return s ? s.charAt(0).toUpperCase() + s.slice(1) : s;
}

/**
 * Split a 400's problems into hard errors (block save) and warnings (allow save),
 * so a surface can present them per the design rule: errors stop, warnings inform.
 */
export function splitProblems(problems: Problem[] | undefined): {
  errors: Problem[];
  warnings: Problem[];
} {
  const errors: Problem[] = [];
  const warnings: Problem[] = [];
  for (const p of problems ?? []) {
    if (p.Severity === "warning") warnings.push(p);
    else errors.push(p);
  }
  return { errors, warnings };
}

/** The rig id embedded in a validation Field like "rigs[rig-01].grace", or null. */
export function rigIdFromField(field: string): string | null {
  const m = /^rigs\[([^\]]+)\]/.exec(field);
  return m?.[1] ?? null;
}

/** Screen-reader wording for an unacknowledged-alert count badge. */
export function unackedAlertsLabel(n: number): string {
  return `${n} unacknowledged ${n === 1 ? "alert" : "alerts"}`;
}
