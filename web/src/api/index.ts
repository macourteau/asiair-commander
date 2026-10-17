/**
 * API barrel. Surfaces import the client, types, and SSE hooks from here:
 *
 *   import { api, ApiError, useSnapshot, type RigStatus } from "@/api";
 */
export {
  api,
  ApiError,
  UNAUTHORIZED_EVENT,
  frameHasImage,
  frameImageUrl,
  rigPath,
  type Api,
  type AckFrameChallenge,
  type AckAlready,
  type AckGroupSkipped,
  type AckGroupChallenge,
} from "./client";
export { previewRetarget, applyRetarget } from "./retarget";
export { previewRefile, applyRefile } from "./refile";
export {
  listDeviceFiles,
  deviceDownloadUrl,
  checkDeviceDownload,
  makeDeviceFolder,
  planDeviceDelete,
  runDeviceDelete,
  planDeviceCopy,
  runDeviceCopy,
  renameDeviceEntry,
  stageUpload,
  planDeviceUpload,
  runDeviceUpload,
  discardUpload,
} from "./deviceFiles";
export { CSRF_HEADER, readCsrfToken } from "./csrf";
export {
  EventsProvider,
  useSnapshot,
  useServerInfo,
  useConnState,
  useEventSubscription,
  type ConnState,
  type EventName,
} from "./useEvents";
export * from "./types";
