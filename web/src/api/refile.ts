/**
 * Re-file API — the Tools page's re-file endpoints (design
 * routing-and-filing §8.11). Both are POST and both go through the
 * write guard, so both carry CSRF; the fetch and NDJSON boilerplate is shared with
 * retarget.ts via stream.ts.
 *
 *   POST /api/rigs/{id}/refile/preview  body {scope}              -> RefilePreview
 *   POST /api/rigs/{id}/refile/apply    body {to_dir, frame_ids}  -> streamed
 *     NDJSON: one RefileResult per FILE, then a final RefileSummary.
 *
 * One request carries one group. A group runs to completion on the worker without
 * yielding, so its result lines arrive as a burst at the end — progress is
 * per-group, and a caller with several groups selected applies them in sequence.
 */
import { rigPath } from "./client";
import { postJson, postNdjson } from "./stream";
import type {
  RefilePreview,
  RefileResult,
  RefileScope,
  RefileSummary,
} from "./types";

/**
 * The two shapes an apply stream carries. They are distinguished structurally
 * rather than by position: the server writes the summary last, but it also writes
 * a failed result line ahead of the results when the group itself errored, so
 * "the last line" is the only thing position guarantees.
 */
type RefileLine = RefileResult | RefileSummary;

function isSummary(line: RefileLine): line is RefileSummary {
  return !("status" in line);
}

/**
 * Dry-run the migration for one rig: every eligible frame whose routed directory
 * differs from its stored one, grouped old dir → new dir, plus the paths refused
 * and the directories the prune expects to be unable to remove. Read-only, and a
 * whole-library scan — pass a signal so a slow one can be abandoned.
 */
export function previewRefile(
  rigId: string,
  scope: RefileScope,
  signal?: AbortSignal,
): Promise<RefilePreview> {
  return postJson<RefilePreview>(
    `${rigPath(rigId)}/refile/preview`,
    { scope },
    signal,
  );
}

/**
 * Move one selected group. `onResult` fires per file as the stream is read, and
 * the promise resolves with the run's summary — the prune outcomes, which are
 * directories rather than frames and so cannot arrive as results. Resolves null
 * only if the stream ended without one, which the server does not do.
 *
 * Cancelling `signal` aborts the request, and the server stops the migration
 * between frames. There is no resume state: previewing again shows what remains.
 */
export async function applyRefile(
  rigId: string,
  toDir: string,
  frameIds: number[],
  onResult: (result: RefileResult) => void,
  signal?: AbortSignal,
): Promise<RefileSummary | null> {
  const summaries: RefileSummary[] = [];
  await postNdjson<RefileLine>(
    `${rigPath(rigId)}/refile/apply`,
    { to_dir: toDir, frame_ids: frameIds },
    (line) => {
      if (isSummary(line)) summaries.push(line);
      else onResult(line);
    },
    signal,
  );
  return summaries.length > 0 ? summaries[summaries.length - 1]! : null;
}
