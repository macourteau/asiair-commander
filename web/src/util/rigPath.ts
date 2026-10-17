/**
 * rigPath answers, inside the rig editor, the question the rest of the editor leaves
 * open: which folder do this rig's frames land in?
 *
 * Nothing in a destination path follows the rig's NAME. The light template names
 * <OTA>, <SESSION> and <RIG>, and those resolve to the rig's OTA, its session
 * label and its immutable id — never to the friendly name shown on the dashboard
 * and the live feed. So renaming a rig after swapping scopes renames it
 * everywhere a person looks and moves nothing on disk, which reads as frames
 * being filed under the old scope "still".
 *
 * Two pieces of copy close that gap: a preview of the folder the current field
 * values produce, and a note on a rename that left the OTA alone — carrying an
 * offer to adopt the new name when the old one was plainly the scope's.
 */

/** The rig-derived tokens; every other token is left standing in the preview. */
type RigTokens = { ota: string; session: string; rigId: string };

/**
 * rigFolderPreview renders `template` with only the rig-derived tokens filled
 * in, so the result reads as a real path where the rig decides it and as a token
 * where the frame does (`<TARGET>/50P/Summer2026/NIGHT_<n>`). Resolving the rest
 * would take a frame header, and the half that matters here is the half the
 * editor controls.
 *
 * A blank value leaves its token in place rather than collapsing the segment:
 * blank OTA is already a hard validation error, and a preview that quietly
 * dropped the segment would show a path the rig will never write to.
 *
 * Backslash-authored templates are normalised to forward slashes — the same
 * reading route.SplitAtNight gives them — so the preview does not depend on
 * which separator the operator happened to type.
 */
export function rigFolderPreview(template: string, t: RigTokens): string {
  const tmpl = template.trim();
  if (!tmpl) return "";
  const value: Record<string, string> = {
    "<OTA>": t.ota.trim(),
    "<SESSION>": t.session.trim(),
    "<RIG>": t.rigId.trim(),
  };
  return tmpl
    .replace(/\\/g, "/")
    .replace(/<OTA>|<SESSION>|<RIG>/g, (tok) => value[tok] || tok);
}

/** What a rename leaves unsaid. `suggestion` is set only when the OTA is
 * unmistakably the old name, so it is safe to offer as one click. */
export interface RenameNote {
  suggestion: string | null;
}

/**
 * otaRenameNote decides whether a rename needs saying something about, and
 * whether the new name can be offered as the new OTA.
 *
 * The note fires on the broad shape — the name changed and the OTA did not —
 * because that is exactly the edit whose whole visible effect is a relabel: the
 * dashboard, the live feed and the frame detail all start saying the new name,
 * and every frame keeps landing in the old scope's folder. Someone renaming a
 * rig for a reason that has nothing to do with optics reads one factual line
 * confirming their folders are untouched, which is worth the interruption
 * against a night filed under the wrong scope.
 *
 * The one-click suggestion is held to a much stricter shape: the OTA must spell
 * the name being renamed away from. That is the operator who named the rig after
 * its scope, where the new name IS the new OTA. A rig called "FRA300 Rig" gets
 * the note and no button, because "50P Rig" is a rig name and not a scope, and a
 * button that writes it would be worse than no button.
 *
 * Matching folds case and drops separators, so "FRA 300" and "fra-300" count as
 * the same spelling. This decides whether to OFFER a value, never what to save.
 */
export function otaRenameNote(
  prev: { name: string; ota: string },
  next: { name: string; ota: string },
): RenameNote | null {
  const prevName = prev.name.trim();
  const nextName = next.name.trim();
  const ota = next.ota.trim();
  if (!prevName || !nextName) return null;
  if (fold(prevName) === fold(nextName)) return null; // not a rename
  if (ota !== prev.ota.trim()) return null; // the OTA is being edited too
  if (!ota) return null; // blank OTA is already a hard validation error
  const named = fold(ota) === fold(prevName) && fold(ota) !== fold(nextName);
  return { suggestion: named ? nextName : null };
}

/** Case- and separator-insensitive spelling of a name, for hint matching only. */
function fold(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9]+/g, "");
}
