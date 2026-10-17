/**
 * The plumbing shared by the endpoints that can't go through client.ts's
 * `request()`: the Tools page's retarget and re-file runs and the File browser's runs answer with
 * streamed NDJSON rather than one JSON body, so they need their own fetch and an
 * incremental line reader. Their previews sit here too, so a caller sees the same
 * ApiError — message, `problems`, `code` — whichever door it came through.
 *
 * The upload's staging request is here too, as an XMLHttpRequest: `fetch` reports
 * no upload progress.
 *
 * Line shapes are the caller's business: `postNdjson` is generic, so a stream
 * whose final line differs from the rest (re-file's summary) narrows in the
 * caller rather than being widened to `any` here.
 */
import { CSRF_HEADER, readCsrfToken } from "./csrf";
import { ApiError, UNAUTHORIZED_EVENT } from "./client";
import type { Problem, ProblemsResponse } from "./types";

/** Mirrors client.ts's errorBody handling, `problems` included: a re-file 400 can
 * carry field-level findings and dropping them would leave the surface with a bare
 * sentence it can't attach to anything. */
async function throwHttpError(res: Response): Promise<never> {
  let text = "";
  try {
    text = await res.text();
  } catch {
    /* unreadable body: worded from the status alone */
  }
  throw httpError(res.status, res.statusText, text);
}

/** The ApiError for a failed response. `body` stays unset when the response was
 * not JSON, which is how a caller tells a proxy's answer from Commander's. */
function httpError(status: number, statusText: string, text: string): ApiError {
  let message = `Request failed (${status} ${statusText}).`;
  let problems: Problem[] | undefined;
  let code: string | undefined;
  let raw: Record<string, unknown> | undefined;
  try {
    const body = JSON.parse(text) as Partial<ProblemsResponse> & {
      error?: string;
      message?: string;
      code?: string;
    };
    message = body?.message || body?.error || message;
    problems = Array.isArray(body?.problems) ? body.problems : undefined;
    code = body?.code;
    if (body && typeof body === "object") raw = body as Record<string, unknown>;
  } catch {
    /* non-JSON error body */
  }
  return new ApiError(status, message, { problems, code, body: raw });
}

function unauthorized(): ApiError {
  window.dispatchEvent(new CustomEvent(UNAUTHORIZED_EVENT));
  return new ApiError(401, "Your session has expired. Please sign in again.");
}

function checkUnauthorized(res: Response): void {
  if (res.status === 401) throw unauthorized();
}

async function post(
  path: string,
  body: unknown,
  accept: string,
  signal?: AbortSignal,
): Promise<Response> {
  const res = await fetch(path, {
    method: "POST",
    credentials: "same-origin",
    headers: {
      Accept: accept,
      "Content-Type": "application/json",
      [CSRF_HEADER]: readCsrfToken(),
    },
    body: JSON.stringify(body),
    signal,
  });
  checkUnauthorized(res);
  if (!res.ok) await throwHttpError(res);
  return res;
}

/** GET a JSON response through the same error path as the streamed endpoints,
 * so a device-files refusal keeps its whole body (see ApiError.body). */
export async function getJson<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(path, {
    method: "GET",
    credentials: "same-origin",
    headers: { Accept: "application/json" },
    signal,
  });
  checkUnauthorized(res);
  if (!res.ok) await throwHttpError(res);
  return (await res.json()) as T;
}

/** POST a JSON body and parse a JSON response; an empty body resolves undefined,
 * as client.ts's `request()` does. */
export async function postJson<T>(
  path: string,
  body: unknown,
  signal?: AbortSignal,
): Promise<T> {
  const res = await post(path, body, "application/json", signal);
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

/**
 * POST a JSON body and read the NDJSON response line by line, invoking `onLine`
 * as each arrives rather than after the whole stream completes — that is what
 * lets a caller drive live progress. Resolves once the stream ends; rejects with
 * ApiError if the request itself failed before streaming began, or with the
 * AbortError of `signal` when the caller cancels.
 */
export async function postNdjson<T>(
  path: string,
  body: unknown,
  onLine: (line: T) => void,
  signal?: AbortSignal,
): Promise<void> {
  const res = await post(path, body, "application/x-ndjson", signal);
  if (!res.body) return;

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    let nl: number;
    while ((nl = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, nl).trim();
      buf = buf.slice(nl + 1);
      if (line) onLine(JSON.parse(line) as T);
    }
  }
  const tail = buf.trim();
  if (tail) onLine(JSON.parse(tail) as T);
}

/**
 * POST a file as the raw request body and parse the JSON answer, reporting the
 * bytes sent through `onProgress` as the browser sends them. A refusal rejects
 * with the same ApiError as `postJson`, a 401 routes through the sign-in
 * redirect like every other request, a cancel through `signal` rejects with an
 * AbortError, and a connection that breaks off rejects with a TypeError, as
 * `fetch` does: the server aborts a body it could not take in full rather than
 * answering it.
 */
export function postFile<T>(
  path: string,
  file: Blob,
  onProgress: (loaded: number, total: number) => void,
  signal?: AbortSignal,
): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const abortError = () => new DOMException("The operation was aborted.", "AbortError");
    if (signal?.aborted) {
      reject(abortError());
      return;
    }
    const xhr = new XMLHttpRequest();
    const onAbort = () => xhr.abort();
    signal?.addEventListener("abort", onAbort);
    const done = () => signal?.removeEventListener("abort", onAbort);

    xhr.open("POST", path);
    xhr.setRequestHeader("Accept", "application/json");
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    xhr.setRequestHeader(CSRF_HEADER, readCsrfToken());
    xhr.upload.onprogress = (e) => onProgress(e.loaded, e.lengthComputable ? e.total : file.size);
    xhr.onload = () => {
      done();
      if (xhr.status === 401) {
        reject(unauthorized());
        return;
      }
      if (xhr.status < 200 || xhr.status >= 300) {
        reject(httpError(xhr.status, xhr.statusText, xhr.responseText));
        return;
      }
      try {
        resolve((xhr.responseText ? JSON.parse(xhr.responseText) : undefined) as T);
      } catch (err) {
        reject(err);
      }
    };
    xhr.onerror = () => {
      done();
      reject(new TypeError("The connection to Commander was lost."));
    };
    xhr.onabort = () => {
      done();
      reject(abortError());
    };
    xhr.send(file);
  });
}
