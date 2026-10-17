/**
 * Retarget API — the Tools page's frame-retargeting endpoints (design
 * routing-and-filing §9.6). Split out from client.ts because Apply's
 * response is streamed NDJSON, not a single JSON body, so it can't go through the
 * shared `request()` helper; the fetch, CSRF and line-reading boilerplate lives in
 * stream.ts, which refile.ts shares.
 *
 *   POST /api/rigs/{id}/retarget/preview  body {ids, target} -> RetargetPreview[]
 *   POST /api/rigs/{id}/retarget/apply    body {ids, target} -> streamed NDJSON,
 *     one RetargetResult per line, flushed as each frame completes.
 */
import { rigPath } from "./client";
import { postJson, postNdjson } from "./stream";
import type { RetargetPreview, RetargetResult } from "./types";

/**
 * Dry-run a retarget: for each id, where it would move and whether it's eligible
 * (read-only, no NAS/device I/O server-side).
 */
export async function previewRetarget(
  rigId: string,
  ids: number[],
  target: string,
  signal?: AbortSignal,
): Promise<RetargetPreview[]> {
  const rows = await postJson<RetargetPreview[]>(
    `${rigPath(rigId)}/retarget/preview`,
    { ids, target },
    signal,
  );
  return rows ?? [];
}

/**
 * Apply a retarget across `ids`. The response streams one RetargetResult per line
 * as each frame is retargeted on the worker goroutine, so `onResult` fires
 * incrementally rather than after the whole batch completes — the caller drives a
 * live per-frame progress view from it. Resolves once the stream ends; rejects
 * with ApiError if the request itself failed (a non-2xx before streaming began).
 */
export function applyRetarget(
  rigId: string,
  ids: number[],
  target: string,
  onResult: (result: RetargetResult) => void,
  signal?: AbortSignal,
): Promise<void> {
  return postNdjson<RetargetResult>(
    `${rigPath(rigId)}/retarget/apply`,
    { ids, target },
    onResult,
    signal,
  );
}
