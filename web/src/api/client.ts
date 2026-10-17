/**
 * The API client — the ONE module that knows every endpoint path and its
 * request/response types. Nothing else in the SPA constructs an /api URL. If the
 * backend renames a path, it changes here and nowhere else.
 *
 * Cross-cutting behavior:
 *  - credentials are sent (session cookie) on every request;
 *  - mutations (POST/PUT/PATCH/DELETE) attach the X-CSRF-Token header from the cookie;
 *  - a 401 dispatches a window "aac:unauthorized" event so AuthContext can bounce
 *    the user to the login screen, and throws ApiError so callers can stop;
 *  - non-2xx responses throw ApiError carrying the parsed { message } text and,
 *    when the server sends one, the validation `problems` array, so surfaces can
 *    show "what happened + what to do" and mark the offending fields.
 *
 * ─── Reconciled against the Go backend (internal/web) ───
 *  - Bootstrap is two probes, not an overloaded /healthz: GET /api/setup →
 *    { needed } (first-run status, unauthenticated) and GET /api/config (200 when
 *    authed, 401 when not) via probeSession().
 *  - POST /api/setup body is { password, config?, allowed_hosts? }, carries the
 *    one-time token in X-Setup-Token, and returns { status, config, seeded_host,
 *    seeded_host_is_ip }.
 *  - Collection GETs: /api/rigs, /api/alerts, /api/discovered, /api/scan, and both
 *    log endpoints return BARE arrays; /api/rigs/{id}/frames is
 *    { frames, page, page_size, total }; /api/config is the redacted Config.
 *  - Commands are colon-verb single segments: /api/rigs/{id}:pause|:resume|
 *    :run-now|:reclassify and /api/alerts/{id}:ack. They 202/200 with a small JSON
 *    body callers ignore.
 *  - Secrets round-trip via the redaction sentinel (types.REDACTED_SENTINEL);
 *    config PUT uses optimistic concurrency (echo updated_at → 409 if stale) and a
 *    400 carries { error, problems }.
 */
import { CSRF_HEADER, readCsrfToken } from "./csrf";
import type {
  Alert,
  AppLogEntry,
  Config,
  DeviceShares,
  DiscoveredRig,
  Frame,
  FramesPage,
  FrameState,
  LibraryStatus,
  LogPull,
  MissingFramesPage,
  PreviewSize,
  PreviewRequest,
  PreviewResponse,
  Problem,
  ProblemsResponse,
  RemotePairing,
  RemoteRegion,
  RemoteStatus,
  RigProfile,
  RigSummary,
  Roots,
  RootsCheckResponse,
  SetupResponse,
  SetupStatus,
} from "./types";

/** Fired on any 401 so the auth layer can redirect to login. */
export const UNAUTHORIZED_EVENT = "aac:unauthorized";

/** Carries the one-time first-run token on POST /api/setup. */
const SETUP_TOKEN_HEADER = "X-Setup-Token";

export class ApiError extends Error {
  readonly status: number;
  /** Retry-After seconds if the server rate-limited the request (login backoff). */
  readonly retryAfter?: number;
  /** Validation findings from a 400 body (config PUT / setup / rig CRUD). */
  readonly problems?: Problem[];
  /**
   * Machine-readable refusal code from the body, when the server sends one (e.g.
   * SETUP_TOKEN_REQUIRED / SETUP_COMPLETE). Callers branch on this rather than on
   * the prose, which is free to change.
   */
  readonly code?: string;
  /** The parsed error body, for a refusal that carries data beside its code (a
   * device-files `409 unarchived` carries the fresh count). Set by stream.ts only. */
  readonly body?: Record<string, unknown>;
  constructor(
    status: number,
    message: string,
    opts: {
      retryAfter?: number;
      problems?: Problem[];
      code?: string;
      body?: Record<string, unknown>;
    } = {},
  ) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.retryAfter = opts.retryAfter;
    this.problems = opts.problems;
    this.code = opts.code;
    this.body = opts.body;
  }
}

/** The server's description of what acknowledging one frame would do (mint response). */
export interface AckFrameChallenge {
  token: string;
  expires_at: number;
  dest_path: string;
  source_path: string;
  state: string;
}

/** Returned by a challenge whose target is already acknowledged — no token minted. */
export interface AckAlready {
  already: true;
}

/** Rows under the directory that don't qualify, so a mint's `count` can be explained. */
export interface AckGroupSkipped {
  restore_pending: number;
  not_verified: number;
  already: number;
}

/** The server's description of what acknowledging a whole directory would do (mint response). */
export interface AckGroupChallenge {
  token: string;
  expires_at: number;
  dir: string;
  count: number;
  dir_gone: boolean;
  skipped: AckGroupSkipped;
}

type Method = "GET" | "POST" | "PUT" | "PATCH" | "DELETE";

interface RequestOpts {
  method?: Method;
  body?: unknown;
  /** Suppress the global 401 → redirect (used by the bootstrap/login probes). */
  allow401?: boolean;
  /** Extra request headers (the setup POST's one-time token). */
  headers?: Record<string, string>;
  signal?: AbortSignal;
}

async function request<T>(path: string, opts: RequestOpts = {}): Promise<T> {
  const method = opts.method ?? "GET";
  const headers: Record<string, string> = {
    Accept: "application/json",
    ...opts.headers,
  };
  const init: RequestInit = { method, headers, credentials: "same-origin" };

  if (opts.body !== undefined) {
    headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(opts.body);
  }
  if (method !== "GET") {
    headers[CSRF_HEADER] = readCsrfToken();
  }
  if (opts.signal) init.signal = opts.signal;

  const res = await fetch(path, init);

  if (res.status === 401 && !opts.allow401) {
    window.dispatchEvent(new CustomEvent(UNAUTHORIZED_EVENT));
    throw new ApiError(401, "Your session has expired. Please sign in again.");
  }

  if (!res.ok) {
    const retryAfter = parseRetryAfter(res.headers.get("Retry-After"));
    const { message, problems, code } = await errorBody(res);
    throw new ApiError(res.status, message, { retryAfter, problems, code });
  }

  if (res.status === 204) return undefined as T;
  const text = await res.text();
  if (!text) return undefined as T;
  return JSON.parse(text) as T;
}

function parseRetryAfter(v: string | null): number | undefined {
  if (!v) return undefined;
  const n = Number(v);
  return Number.isFinite(n) ? n : undefined;
}

async function errorBody(
  res: Response,
): Promise<{ message: string; problems?: Problem[]; code?: string }> {
  try {
    const body = (await res.json()) as Partial<ProblemsResponse> & {
      error?: string;
      message?: string;
      code?: string;
    };
    const message =
      body?.message ||
      body?.error ||
      `Request failed (${res.status} ${res.statusText}).`;
    const problems = Array.isArray(body?.problems) ? body.problems : undefined;
    return { message, problems, code: body?.code };
  } catch {
    return { message: `Request failed (${res.status} ${res.statusText}).` };
  }
}

function qs(params: Record<string, string | number | boolean | undefined>): string {
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== "") sp.set(k, String(v));
  }
  const s = sp.toString();
  return s ? `?${s}` : "";
}

/** Exported so the retarget module (retarget.ts) can build its own request paths
 * without duplicating the encoding rule. */
export const rigPath = (id: string) => `/api/rigs/${encodeURIComponent(id)}`;

const remotePath = (id: string) => `/api/remotes/${encodeURIComponent(id)}`;

/** Frame states whose bytes are on disk, so a preview can be rendered. Mirrors the
 * backend eligibility gate in internal/web/image_api.go. */
const IMAGE_STATES: ReadonlySet<FrameState> = new Set([
  "verified",
  "deleted",
  "unclassified",
  "held",
]);

/** Whether a frame has an on-disk copy a preview can be generated from. */
export function frameHasImage(
  frame: Pick<Frame, "state" | "dest_path" | "sha256"> | null | undefined,
): boolean {
  return (
    !!frame && IMAGE_STATES.has(frame.state) && !!frame.dest_path && !!frame.sha256
  );
}

/**
 * URL for a frame's preview image, for use as an `<img src>` (the session cookie
 * rides automatically, same-origin). The `v=` content stamp makes the endpoint's
 * year-long immutable cache safe: a re-pulled frame (new sha256, same id) gets a
 * new URL rather than a stale hit. Pass a numeric {w,h} box for an uncached,
 * never-upscaled arbitrary size instead of a named one.
 */
export function frameImageUrl(
  frame: Pick<Frame, "id" | "sha256">,
  size: PreviewSize | { w?: number; h?: number } = "thumb",
): string {
  const stamp = (frame.sha256 ?? "").slice(0, 12);
  if (typeof size === "string") {
    return `/api/frames/${frame.id}/image${qs({ size, v: stamp })}`;
  }
  return `/api/frames/${frame.id}/image${qs({ w: size.w, h: size.h, v: stamp })}`;
}

/** Every endpoint the SPA can call, grouped by surface. Paths live only here. */
export const api = {
  // ---- bootstrap (unauthenticated) --------------------------------------
  /** First-run status: is a password still unset? */
  getSetupStatus: (signal?: AbortSignal) =>
    request<SetupStatus>("/api/setup", { allow401: true, signal }),
  /**
   * Probe the session WITHOUT the global 401→login redirect: GET /api/config
   * answers 200 when authed and 401 when not. Returns the boolean; other errors
   * propagate so a real outage is not mistaken for "logged out".
   */
  probeSession: async (signal?: AbortSignal): Promise<boolean> => {
    try {
      await request<Config>("/api/config", { allow401: true, signal });
      return true;
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) return false;
      throw err;
    }
  },

  // ---- session ----------------------------------------------------------
  login: (password: string) =>
    request<void>("/api/session", {
      method: "POST",
      body: { password },
      allow401: true, // a wrong password is a 401 we handle inline, not a redirect
    }),
  logout: () => request<void>("/api/session", { method: "DELETE" }),

  /**
   * First-run wizard. The body is { password, config?, allowed_hosts? }: a nil
   * config seeds full shipped defaults. Returns the seeded, redacted config so the
   * caller can layer roots/first-rig via a follow-up PUT.
   *
   * The one-time token is required of every client, loopback included, and rides
   * its own header rather than the body so it is never persisted in a config or
   * echoed in a validation error. A 403 carries `code`: SETUP_TOKEN_REQUIRED for a
   * missing/wrong token, SETUP_COMPLETE once the window has closed.
   */
  setup: (
    body: { password: string; config?: Config; allowed_hosts?: string[] },
    setupToken?: string,
  ) =>
    request<SetupResponse>("/api/setup", {
      method: "POST",
      body,
      allow401: true,
      headers: setupToken ? { [SETUP_TOKEN_HEADER]: setupToken } : undefined,
    }),

  // ---- config -----------------------------------------------------------
  /**
   * Load the whole config. Like putConfig, the body carries an added `problems`
   * array — findings about what is already stored, so a hand-edited address list
   * is flagged on open. Strip it before PUTting the config back: the server
   * rejects unknown fields.
   */
  getConfig: (signal?: AbortSignal) =>
    request<Config & { problems?: Problem[] }>("/api/config", { signal }),
  /**
   * Save the whole config. The 200 body is the redacted config with an added
   * `problems` array carrying the non-blocking findings about what was saved
   * (a stored address-list entry that validation would refuse warns instead of
   * blocking, so this is the only channel that tells the operator to fix it).
   * Strip `problems` before echoing the config back in a later PUT — the server
   * rejects unknown fields.
   */
  putConfig: (config: Config) =>
    request<Config & { problems?: Problem[] }>("/api/config", {
      method: "PUT",
      body: config,
    }),
  /**
   * Dry-run templates against real recent frames (design architecture §9 live
   * preview). This endpoint may be absent on older backends; callers must degrade
   * gracefully on a 404.
   */
  previewConfig: (body: PreviewRequest, signal?: AbortSignal) =>
    request<PreviewResponse>("/api/config/preview", { method: "POST", body, signal }),
  /**
   * Probe whether the SERVICE ACCOUNT can write the candidate destination roots
   * (it runs the MkdirAll + write-probe in-process on the NAS). The wizard blocks
   * setup until every root is writable and uses the returned account name to tell
   * the operator which account to grant access to.
   */
  checkRoots: (roots: Partial<Roots>) =>
    request<RootsCheckResponse>("/api/roots/check", { method: "POST", body: { roots } }),

  // ---- rigs -------------------------------------------------------------
  listRigs: (signal?: AbortSignal) =>
    request<RigProfile[]>("/api/rigs", { signal }),
  getRig: (id: string, signal?: AbortSignal) =>
    request<RigProfile>(rigPath(id), { signal }),
  createRig: (rig: RigProfile) =>
    request<RigProfile>("/api/rigs", { method: "POST", body: rig }),
  updateRig: (id: string, rig: RigProfile) =>
    request<RigProfile>(rigPath(id), { method: "PUT", body: rig }),
  deleteRig: (id: string) => request<void>(rigPath(id), { method: "DELETE" }),

  // ---- rig commands (fire-and-return) -----------------------------------
  pauseRig: (id: string) =>
    request<void>(`${rigPath(id)}:pause`, { method: "POST" }),
  resumeRig: (id: string) =>
    request<void>(`${rigPath(id)}:resume`, { method: "POST" }),
  runRigNow: (id: string) =>
    request<void>(`${rigPath(id)}:run-now`, { method: "POST" }),
  /** Ends a paused rig's "run until idle" at the next frame boundary; the rig stays paused. */
  stopRun: (id: string) =>
    request<void>(`${rigPath(id)}:stop-run`, { method: "POST" }),
  reclassifyRig: (id: string) =>
    request<void>(`${rigPath(id)}:reclassify`, { method: "POST" }),
  /** Dismiss one permanently-stuck unclassified frame: purges its NAS copy and tombstones the row. */
  discardFrame: (id: string, frameId: number) =>
    request<void>(`${rigPath(id)}/frames/${frameId}`, { method: "DELETE" }),
  resetRigIdentity: (id: string) =>
    request<void>(`${rigPath(id)}:reset-identity`, { method: "POST" }),

  // ---- library reconciliation (out-of-band deletion) --------------------
  /**
   * Every configured destination root's link state, for the global Library section
   * in Settings. Linking is automatic on the first filed frame; this surfaces a root
   * whose marker went missing (a deleted sentinel) so it can be re-linked.
   */
  libraryStatus: (signal?: AbortSignal) =>
    request<LibraryStatus>("/api/library/status", { signal }),
  /**
   * Re-link a destination root: rewrites its `.asiair-commander-library-id` marker.
   * The endpoint is per-rig but the operation is global to the root (the marker keys
   * the root, not the rig), so any managed rig's id links it for the whole fleet.
   */
  linkLibrary: (id: string, root: string) =>
    request<void>(`${rigPath(id)}/library/link`, { method: "POST", body: { root } }),
  /** Kick off a library reconciliation scan; 202 fire-and-return (progress rides the snapshot). */
  rescanLibrary: (id: string) =>
    request<void>(`${rigPath(id)}/library/scan`, { method: "POST" }),
  /** Request a re-pull of a frame whose filed copy went missing from the NAS. */
  restoreFrame: (id: string, frameId: number) =>
    request<void>(`${rigPath(id)}/frames/${frameId}/restore`, { method: "POST" }),
  /** Cancel a pending restore before the re-pull runs. */
  cancelRestore: (id: string, frameId: number) =>
    request<void>(`${rigPath(id)}/frames/${frameId}/restore`, { method: "DELETE" }),
  /**
   * Mint a confirmation for acknowledging one culled frame (design
   * library-reconciliation §10.7): the operator is asserting the
   * NAS copy was deleted on purpose, waiving the reconcile-settle window. Returns
   * the server's description of what would happen, or `{already: true}` when the
   * frame is already acknowledged (no token minted). Throws ApiError with a
   * `.code` (ACK_KEEP_ORIGINALS / ACK_NOT_MISSING / ACK_NOT_VERIFIED /
   * ACK_RESTORE_PENDING) when it cannot be acknowledged at all.
   */
  acknowledgeFrameChallenge: (id: string, frameId: number) =>
    request<AckFrameChallenge | AckAlready>(
      `${rigPath(id)}/frames/${frameId}/acknowledge`,
      { method: "POST", body: {} },
    ),
  /**
   * Confirm a frame acknowledgement with the token from the challenge.
   * `undefined` on success (204); `{code: "ACK_QUEUED"}` on 202 when the rig was
   * busy and the request may already have applied — this is a SUCCESS value, not
   * an ApiError (design library-reconciliation §10.7). Errors (ApiError.code):
   * ACK_CONFIRMATION_INVALID (403, the token expired/was wrong — no write
   * happened) and the same 409 codes as the challenge.
   */
  acknowledgeFrameConfirm: (id: string, frameId: number, token: string) =>
    request<{ code?: string } | undefined>(
      `${rigPath(id)}/frames/${frameId}/acknowledge`,
      { method: "POST", body: { token } },
    ),
  /**
   * Mint a confirmation for acknowledging every acknowledgeable culled frame
   * under one directory. The token binds the candidate frame ids computed at
   * mint time (design library-reconciliation §10.7), so growth in the directory
   * between mint and confirm is not authorized. `count` is the acknowledgeable
   * total; `skipped` explains the gap from the directory's raw tally shown on
   * the group's button.
   */
  acknowledgeGroupChallenge: (id: string, dir: string) =>
    request<AckGroupChallenge>(`${rigPath(id)}/missing-groups/acknowledge`, {
      method: "POST",
      body: { dir },
    }),
  /**
   * Confirm a group acknowledgement with the token from the challenge.
   * `{acknowledged: n}` on success (200) — the server's true count, which may be
   * less than the challenge's `count` if a row was restored or acked meanwhile;
   * `{code: "ACK_QUEUED"}` on 202 (busy, may already have applied — a SUCCESS
   * value, not an ApiError). Errors as for the frame confirm, plus
   * ACK_NOTHING_TO_DO if the directory emptied out before this confirm.
   */
  acknowledgeGroupConfirm: (id: string, dir: string, token: string) =>
    request<{ acknowledged?: number; code?: string } | undefined>(
      `${rigPath(id)}/missing-groups/acknowledge`,
      { method: "POST", body: { dir, token } },
    ),

  // ---- global (fleet-wide) controls ------------------------------------
  pauseAll: () => request<void>("/api/pause", { method: "POST" }),
  resumeAll: () => request<void>("/api/resume", { method: "POST" }),

  // ---- remotes ----------------------------------------------------------
  /** 503 when this build has no remote support. */
  listRemotes: (signal?: AbortSignal) =>
    request<RemoteStatus[]>("/api/remotes", { signal }),
  /** The relay regions, the measured nearest first; may take seconds (a netcheck). */
  listRemoteRegions: (signal?: AbortSignal) =>
    request<RemoteRegion[]>("/api/remotes/regions", { signal }),
  /** region 0 lets the server pick the nearest. The response is shown once. */
  createRemote: (name: string, region: number) =>
    request<RemotePairing>("/api/remotes", { method: "POST", body: { name, region } }),
  renameRemote: (id: string, name: string) =>
    request<RemoteStatus>(remotePath(id), { method: "PATCH", body: { name } }),
  /** mbps 0 turns the cap off. */
  setRemoteRateLimit: (id: string, mbps: number) =>
    request<RemoteStatus>(`${remotePath(id)}/rate-limit`, { method: "PUT", body: { mbps } }),
  /** Mints a new pairing; the old env stops working. region 0 keeps the current
   * one. The response is shown once. */
  repairRemote: (id: string, region: number) =>
    request<RemotePairing>(`${remotePath(id)}:repair`, { method: "POST", body: { region } }),
  deleteRemote: (id: string) => request<void>(remotePath(id), { method: "DELETE" }),

  // ---- discovery --------------------------------------------------------
  /**
   * Trigger a synchronous scan; the backend returns the discovery registry as
   * the round left it. Entries persist past the device going away — `online` is
   * what says a device answered recently.
   */
  scan: () => request<DiscoveredRig[]>("/api/scan", { method: "POST" }),
  listDiscovered: (signal?: AbortSignal) =>
    request<DiscoveredRig[]>("/api/discovered", { signal }),
  /**
   * Enumerate one discovered device's shares at adopt time (opens a bounded guest
   * SMB session server-side) to prefill the editor's watched-share list. Managed
   * devices are already filtered out of the discovered list, so this never needs
   * to report management state.
   */
  fetchDeviceShares: (guid: string) =>
    request<DeviceShares>(`/api/discovered/${encodeURIComponent(guid)}/shares`, {
      method: "POST",
    }),

  // ---- frames & summary (bulk history via paginated REST, never SSE) -----
  listFrames: (
    id: string,
    params: { state?: FrameState; filter?: string; page?: number; limit?: number } = {},
    signal?: AbortSignal,
  ) => request<FramesPage>(`${rigPath(id)}/frames${qs(params)}`, { signal }),
  /**
   * Frames whose filed NAS copy went missing out of band, paged and grouped by
   * directory. `missing=true` is exclusive of state/filter (the backend 400s if
   * combined), so this is its own method rather than a listFrames flag.
   */
  missingFrames: (id: string, page?: number, signal?: AbortSignal) =>
    request<MissingFramesPage>(
      `${rigPath(id)}/frames${qs({ missing: true, page })}`,
      { signal },
    ),
  /**
   * The combined transfer history across every rig: frames actually pulled to the
   * NAS, newest-first, filterable by rig and/or a pulled_at window (unix seconds).
   */
  listTransferHistory: (
    params: { rig?: string; from?: number; to?: number; page?: number; page_size?: number } = {},
    signal?: AbortSignal,
  ) => request<FramesPage>(`/api/frames${qs(params)}`, { signal }),
  getSummary: (id: string, night?: number, signal?: AbortSignal) =>
    request<RigSummary>(`${rigPath(id)}/summary${qs({ night })}`, { signal }),
  /** A frame's native pixel dimensions, for the lightbox zoom srcSet. */
  imageMeta: (frameId: number, signal?: AbortSignal) =>
    request<{ width: number; height: number }>(`/api/frames/${frameId}/image/meta`, { signal }),

  // ---- plate solving ----------------------------------------------------
  /**
   * Start downloading a Tier 1 star catalog by name (d05/d20/d50). 200
   * {status:"installed"} when already present, 202 {status:"started"} when a
   * download begins, 409 {status:"busy",downloading} when another is in flight,
   * 400 for an unknown name, 503 when plate solving is disabled. Progress rides
   * the live snapshot's solver_status, so callers don't poll.
   */
  downloadCatalog: (name: string) =>
    request<{ status: string; catalog?: string; downloading?: string }>(
      `/api/platesolve/catalogs/${encodeURIComponent(name)}`,
      { method: "POST" },
    ),
  /** Re-queue frames that failed to solve for another attempt (e.g. after installing a deeper catalog). */
  resolveFailed: () =>
    request<{ requeued: number }>("/api/platesolve/resolve", { method: "POST" }),

  // ---- alerts -----------------------------------------------------------
  listAlerts: (
    params: { include_acked?: boolean; limit?: number } = {},
    signal?: AbortSignal,
  ) => request<Alert[]>(`/api/alerts${qs(params)}`, { signal }),
  ackAlert: (id: number) =>
    request<void>(`/api/alerts/${id}:ack`, { method: "POST" }),

  // ---- logs -------------------------------------------------------------
  getRigLogs: (id: string, signal?: AbortSignal) =>
    request<LogPull[]>(`${rigPath(id)}/logs`, { signal }),
  getAppLogs: (
    params: { level?: string; limit?: number } = {},
    signal?: AbortSignal,
  ) => request<AppLogEntry[]>(`/api/app-logs${qs(params)}`, { signal }),
  /**
   * URL of the current (unrotated) app-log file, served as a download by the
   * backend. A plain same-origin GET that carries the session cookie, so a bare
   * <a href download> is enough — no fetch/blob needed. Kept here so this module
   * stays the sole place that knows an /api path.
   */
  appLogDownloadUrl: "/api/app-logs/download",
} as const;

export type Api = typeof api;
