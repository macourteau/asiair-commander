/**
 * Device files API — the File browser page's access to files on the ASIAIRs themselves
 * (design device-file-browser §4).
 *
 *   GET  /api/rigs/{id}/files?share=&dir=                     -> DeviceFileListing
 *   GET  /api/rigs/{id}/files/download?share=&dir=&name=…[&check=1]
 *   POST /api/rigs/{id}/files/mkdir        {share, dir, name}
 *   POST /api/rigs/{id}/files/delete/plan  {share, dir, names} -> DeletePlan
 *   POST /api/rigs/{id}/files/delete       DeleteRunRequest    -> NDJSON
 *   POST /api/files/copy/plan              CopyPlanRequest     -> CopyPlan
 *   POST /api/files/copy                   CopyRunRequest      -> NDJSON
 *   POST /api/rigs/{id}/files/rename       RenameRequest       -> 204
 *   POST /api/files/upload/stage?name=&mtime_ms=  raw file     -> StagedUpload
 *   POST /api/files/upload/plan            UploadPlanRequest   -> UploadPlan
 *   POST /api/files/upload                 UploadRunRequest    -> NDJSON
 *   POST /api/files/upload/discard         {id}                -> 204
 *
 * Every refusal is an ApiError whose `code` names it (busy, not_found, exists,
 * unarchived, …); callers branch on the code, never the prose.
 */
import { rigPath } from "./client";
import { getJson, postFile, postJson, postNdjson } from "./stream";
import type {
  CopyPlan,
  CopyPlanRequest,
  CopyRunRequest,
  DeletePlan,
  DeleteRunRequest,
  DeviceFileListing,
  DownloadCheck,
  FilesRunLine,
  RenameRequest,
  StagedUpload,
  UploadPlan,
  UploadPlanRequest,
  UploadRunRequest,
} from "./types";

function query(pairs: [string, string][]): string {
  const sp = new URLSearchParams();
  for (const [k, v] of pairs) sp.append(k, v);
  const s = sp.toString();
  return s ? `?${s}` : "";
}

/** The share list (no `share`) or one folder of a share. */
export function listDeviceFiles(
  rigId: string,
  share: string | null,
  dir: string,
  signal?: AbortSignal,
): Promise<DeviceFileListing> {
  const pairs: [string, string][] = [];
  if (share !== null) {
    pairs.push(["share", share]);
    if (dir) pairs.push(["dir", dir]);
  }
  return getJson<DeviceFileListing>(`${rigPath(rigId)}/files${query(pairs)}`, signal);
}

/** The download URL for a selection: one `name` per selected item. With `check`
 * the server walks the selection and answers its JSON preview instead. */
export function deviceDownloadUrl(
  rigId: string,
  share: string,
  dir: string,
  names: string[],
  check = false,
): string {
  const pairs: [string, string][] = [
    ["share", share],
    ["dir", dir],
  ];
  for (const n of names) pairs.push(["name", n]);
  if (check) pairs.push(["check", "1"]);
  return `${rigPath(rigId)}/files/download${query(pairs)}`;
}

export function checkDeviceDownload(
  rigId: string,
  share: string,
  dir: string,
  names: string[],
  signal?: AbortSignal,
): Promise<DownloadCheck> {
  return getJson<DownloadCheck>(deviceDownloadUrl(rigId, share, dir, names, true), signal);
}

export function makeDeviceFolder(
  rigId: string,
  share: string,
  dir: string,
  name: string,
): Promise<void> {
  return postJson<void>(`${rigPath(rigId)}/files/mkdir`, { share, dir, name });
}

export function planDeviceDelete(
  rigId: string,
  share: string,
  dir: string,
  names: string[],
  signal?: AbortSignal,
): Promise<DeletePlan> {
  return postJson<DeletePlan>(
    `${rigPath(rigId)}/files/delete/plan`,
    { share, dir, names },
    signal,
  );
}

/** Streams the delete run's lines into `onLine`; resolves when the stream ends,
 * which is with a summary line unless the run was cancelled or cut off. */
export function runDeviceDelete(
  rigId: string,
  body: DeleteRunRequest,
  onLine: (line: FilesRunLine) => void,
  signal?: AbortSignal,
): Promise<void> {
  return postNdjson<FilesRunLine>(`${rigPath(rigId)}/files/delete`, body, onLine, signal);
}

export function planDeviceCopy(
  body: CopyPlanRequest,
  signal?: AbortSignal,
): Promise<CopyPlan> {
  return postJson<CopyPlan>("/api/files/copy/plan", body, signal);
}

/** As runDeviceDelete, for a copy. */
export function runDeviceCopy(
  body: CopyRunRequest,
  onLine: (line: FilesRunLine) => void,
  signal?: AbortSignal,
): Promise<void> {
  return postNdjson<FilesRunLine>("/api/files/copy", body, onLine, signal);
}

export function renameDeviceEntry(
  rigId: string,
  body: RenameRequest,
  signal?: AbortSignal,
): Promise<void> {
  return postJson<void>(`${rigPath(rigId)}/files/rename`, body, signal);
}

/** Sends the picked file to Commander's staging area (phase 1 of an upload),
 * reporting the bytes sent. Nothing reaches a device. */
export function stageUpload(
  file: File,
  onProgress: (loaded: number, total: number) => void,
  signal?: AbortSignal,
): Promise<StagedUpload> {
  const pairs: [string, string][] = [["name", file.name]];
  // No time is better than a wrong one: FAT cannot hold 1970.
  if (file.lastModified > 0) pairs.push(["mtime_ms", String(Math.trunc(file.lastModified))]);
  return postFile<StagedUpload>(`/api/files/upload/stage${query(pairs)}`, file, onProgress, signal);
}

export function planDeviceUpload(
  body: UploadPlanRequest,
  signal?: AbortSignal,
): Promise<UploadPlan> {
  return postJson<UploadPlan>("/api/files/upload/plan", body, signal);
}

/** As runDeviceCopy, for the one staged file of an upload. */
export function runDeviceUpload(
  body: UploadRunRequest,
  onLine: (line: FilesRunLine) => void,
  signal?: AbortSignal,
): Promise<void> {
  return postNdjson<FilesRunLine>("/api/files/upload", body, onLine, signal);
}

/** Asks Commander to drop a staged file; best-effort, the expiry covers a lost
 * request, so the caller ignores the answer. */
export function discardUpload(id: string): Promise<void> {
  return postJson<void>("/api/files/upload/discard", { id });
}
