/**
 * Header-edit API — the Tools page's endpoints for correcting one FITS keyword
 * on archived frames (design header-edits §9.1). Split out from client.ts because
 * Apply's response is streamed NDJSON, not a single JSON body, so it can't go
 * through the shared `request()` helper; the fetch, CSRF and line-reading
 * boilerplate lives in stream.ts, which refile.ts shares.
 *
 *   POST /api/rigs/{id}/edit/preview  body {ids, key, value} -> EditPreview[]
 *   POST /api/rigs/{id}/edit/apply    body {ids, key, value} -> streamed NDJSON,
 *     one EditResult per line, flushed as each frame completes.
 */
import { rigPath } from "./client";
import { postJson, postNdjson } from "./stream";
import type { EditableKey, EditPreview, EditResult } from "./types";

/**
 * Dry-run an edit: for each id, the card's current value, where the frame would
 * be filed and whether it's eligible (read-only, no NAS/device I/O server-side).
 */
export async function previewEdit(
  rigId: string,
  ids: number[],
  key: EditableKey,
  value: string,
  signal?: AbortSignal,
): Promise<EditPreview[]> {
  const rows = await postJson<EditPreview[]>(
    `${rigPath(rigId)}/edit/preview`,
    { ids, key, value },
    signal,
  );
  return rows ?? [];
}

/**
 * Apply an edit across `ids`. The response streams one EditResult per line as
 * each frame is edited on the worker goroutine, so `onResult` fires
 * incrementally rather than after the whole batch completes — the caller drives a
 * live per-frame progress view from it. Resolves once the stream ends; rejects
 * with ApiError if the request itself failed (a non-2xx before streaming began).
 */
export function applyEdit(
  rigId: string,
  ids: number[],
  key: EditableKey,
  value: string,
  onResult: (result: EditResult) => void,
  signal?: AbortSignal,
): Promise<void> {
  return postNdjson<EditResult>(
    `${rigPath(rigId)}/edit/apply`,
    { ids, key, value },
    onResult,
    signal,
  );
}
