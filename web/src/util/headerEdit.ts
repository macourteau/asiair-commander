/**
 * The value rules and copy for the Tools page's Edit header card (design
 * header-edits §3, §9.3). The server enforces the same rules and answers a 400
 * for a value that fails them; mirroring them here lets the card say what is
 * wrong while the operator types, before any request is made.
 *
 * OBJECT is sanitised and folded onto the target's canonical spelling
 * server-side, so the client checks only what it can see: trimmed, non-empty, at
 * most 60 characters. FILTER is stored verbatim, so its character set is checked
 * here too: printable ASCII (0x20–0x7E), the FITS string character set.
 */
import type { EditableKey, Frame } from "@/api";

/** Both keys share the FITS card's value budget (design header-edits §3). */
export const MAX_EDIT_VALUE_LEN = 60;

export interface EditFieldCopy {
  /** The field selector's option label. */
  option: string;
  /** The value input's label. */
  inputLabel: string;
  help: string;
  example: string;
  placeholder: string;
}

export const EDIT_FIELDS: Record<EditableKey, EditFieldCopy> = {
  OBJECT: {
    option: "Target (OBJECT)",
    inputLabel: "New target",
    help: `Replaces the FITS OBJECT header and refiles the frame under this name. Up to ${MAX_EDIT_VALUE_LEN} characters.`,
    example: "'NGC 7000' → 'NGC7000'",
    placeholder: "e.g. NGC7000",
  },
  FILTER: {
    option: "Filter (FILTER)",
    inputLabel: "New filter",
    help: `Replaces the FITS FILTER header exactly as typed, or adds it when the frame has none. The frame is refiled only when its folder template uses the filter. Up to ${MAX_EDIT_VALUE_LEN} plain ASCII characters.`,
    example: "'H-alpha' → 'Ha'",
    placeholder: "e.g. Ha",
  },
};

export const EDITABLE_KEYS = Object.keys(EDIT_FIELDS) as EditableKey[];

/** Counts code points, not UTF-16 units, as the server's rune count does. */
const charCount = (s: string) => [...s].length;

/** A character for an error message: quoted when it can be shown, its code point
 * when it is invisible (a tab, a control character). */
function describeChar(ch: string): string {
  const cp = ch.codePointAt(0)!;
  if (cp < 0x20 || cp === 0x7f) {
    return `the control character U+${cp.toString(16).toUpperCase().padStart(4, "0")}`;
  }
  return `"${ch}"`;
}

/** Non-empty ⇒ why `raw` can't be written to `key` yet. */
export function validateEditValue(key: EditableKey, raw: string): string | undefined {
  const v = raw.trim();
  if (key === "OBJECT") {
    if (!v) return "Enter the corrected target name.";
    const n = charCount(v);
    if (n > MAX_EDIT_VALUE_LEN) {
      return `Target names must be ${MAX_EDIT_VALUE_LEN} characters or fewer (currently ${n}).`;
    }
    return undefined;
  }
  if (!v) return "Enter the corrected filter name.";
  for (const ch of v) {
    const cp = ch.codePointAt(0)!;
    if (cp < 0x20 || cp > 0x7e) {
      return `Filter names can only use plain ASCII letters, digits, spaces and punctuation — ${describeChar(ch)} can't be written to a FITS header.`;
    }
  }
  if (v.length > MAX_EDIT_VALUE_LEN) {
    return `Filter names must be ${MAX_EDIT_VALUE_LEN} characters or fewer (currently ${v.length}).`;
  }
  return undefined;
}

/**
 * The distinct filters in the loaded frame list, sorted, for the FILTER input's
 * suggestions. Spellings that differ only in case are both kept: whether they are
 * one filter is the stacking software's decision, and the operator picking one is
 * the point (design header-edits §9.1).
 */
export function filterSuggestions(frames: Pick<Frame, "filter">[]): string[] {
  const seen = new Set<string>();
  for (const f of frames) {
    if (f.filter && f.filter.trim()) seen.add(f.filter);
  }
  return [...seen].sort((a, b) => a.localeCompare(b));
}
