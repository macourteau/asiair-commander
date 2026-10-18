// @vitest-environment jsdom
/**
 * The device file browser's dialogs against a mocked API: fetch answers JSON for
 * the plans and a hand-driven NDJSON stream for the runs, so each test decides
 * exactly which lines arrive and whether the stream ends with its summary.
 */
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { useState, type ReactNode } from "react";
import { ToastProvider } from "@/design";
import type { CopyPlan, DeletePlan, FilesRunLine, RigStatus, UploadPlan } from "@/api";
import { CopyDialog, DeleteDialog, DeviceFilesPanel } from "./DeviceFiles";

// jsdom does no layout, so offsetParent is null everywhere and the Dialog's focus
// trap would see nothing focusable. An attached element counts as visible.
beforeAll(() => {
  Object.defineProperty(HTMLElement.prototype, "offsetParent", {
    configurable: true,
    get(this: HTMLElement) {
      return this.parentElement;
    },
  });
});

/** An NDJSON response whose lines the test pushes; aborting the request errors
 * the body the way a browser does. */
class Stream {
  private controller!: ReadableStreamDefaultController<Uint8Array>;
  readonly response: Response;
  constructor(signal?: AbortSignal | null) {
    const body = new ReadableStream<Uint8Array>({
      start: (c) => {
        this.controller = c;
      },
    });
    signal?.addEventListener("abort", () => {
      try {
        this.controller.error(new DOMException("The operation was aborted.", "AbortError"));
      } catch {
        /* already closed */
      }
    });
    this.response = new Response(body, {
      status: 200,
      headers: { "Content-Type": "application/x-ndjson" },
    });
  }
  push(line: FilesRunLine) {
    this.controller.enqueue(new TextEncoder().encode(`${JSON.stringify(line)}\n`));
  }
  end() {
    this.controller.close();
  }
}

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

type Handler = (url: string, init: RequestInit) => Response | Promise<Response>;
let routes: { match: (url: string, method: string) => boolean; handle: Handler }[] = [];
const calls: { url: string; method: string; body: unknown }[] = [];
let streams: Stream[] = [];

function route(method: string, path: string | RegExp, handle: Handler) {
  routes.push({
    match: (url, m) =>
      m === method && (typeof path === "string" ? url.split("?")[0] === path : path.test(url)),
    handle,
  });
}

/** A run route whose response is a fresh Stream the test drives. */
function streamRoute(path: string) {
  route("POST", path, (_url, init) => {
    const s = new Stream(init.signal);
    streams.push(s);
    return s.response;
  });
}

beforeEach(() => {
  routes = [];
  calls.length = 0;
  streams = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const url = String(input);
      const method = init.method ?? "GET";
      calls.push({ url, method, body: init.body ? JSON.parse(String(init.body)) : undefined });
      const r = routes.find((x) => x.match(url, method));
      if (!r) throw new Error(`unexpected fetch ${method} ${url}`);
      return r.handle(url, init);
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const withToasts = (ui: ReactNode) => render(<ToastProvider>{ui}</ToastProvider>);

/** Lets queued stream reads and state updates settle. */
async function flush() {
  await act(async () => {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  });
}

const dialog = () => screen.getByRole("dialog");
const focusInDialog = () => dialog().contains(document.activeElement);

/* ---- fixtures ------------------------------------------------------------- */

const from = {
  rig: "a",
  rigName: "Alpha",
  share: "EMMC Images",
  dir: "Preview",
  names: ["ref.fit", "M42"],
};
const to = { rig: "b", rigName: "Bravo", share: "EMMC Images", dir: "Preview" };

function copyPlan(over: Partial<CopyPlan> = {}): CopyPlan {
  return {
    files: 4,
    bytes: 4000,
    // M42 exists at the destination already, so the labels stay plain.
    dirs: [],
    items: [
      { rel: "ref.fit", size: 1000, watched: false },
      { rel: "M42/a.fit", size: 1000, watched: false },
      { rel: "M42/b.fit", size: 1000, watched: false },
      { rel: "M42/c.fit", size: 1000, watched: false },
    ],
    conflicts: [],
    to_dir_missing: 0,
    watched: { frames: 0, previews: 0, keeps_originals: false },
    ...over,
  };
}

function renderCopy(plan: CopyPlan, props: Partial<Parameters<typeof CopyDialog>[0]> = {}) {
  route("POST", "/api/files/copy/plan", () => json(200, plan));
  streamRoute("/api/files/copy");
  const onClose = vi.fn();
  const onRunning = vi.fn();
  const onRelist = vi.fn();
  withToasts(
    <CopyDialog
      source={{ kind: "copy", from, mirror: false }}
      to={to}
      onClose={onClose}
      onRunning={onRunning}
      onRelist={onRelist}
      {...props}
    />,
  );
  return { onClose, onRunning, onRelist };
}

function deletePlan(over: Partial<DeletePlan> = {}): DeletePlan {
  return {
    dirs: 1,
    files: 3,
    bytes: 3000,
    by_ext: [{ ext: "fit", files: 3, bytes: 3000 }],
    unarchived: 3,
    unwatched_files: 0,
    watched_roots: [],
    items: [
      { rel: "M42/a.fit", size: 1000, mtime: 1 },
      { rel: "M42/b.fit", size: 1000, mtime: 1 },
      { rel: "M42/c.fit", size: 1000, mtime: 1 },
    ],
    dirs_deepest_first: ["M42"],
    ...over,
  };
}

function renderDelete(plan: DeletePlan) {
  route("POST", "/api/rigs/a/files/delete/plan", () => json(200, plan));
  const onClose = vi.fn();
  withToasts(
    <DeleteDialog
      rig="a"
      rigName="Alpha"
      share="EMMC Images"
      dir="Preview"
      names={["M42"]}
      onClose={onClose}
      onRunning={() => {}}
      onRelist={() => {}}
    />,
  );
  return { onClose };
}

/* ---- copy ----------------------------------------------------------------- */

describe("CopyDialog", () => {
  it("sends exactly the overwrites the user chose, bound to the state they saw", async () => {
    const plan = copyPlan({
      items: [
        { rel: "ref.fit", size: 1000, watched: false },
        { rel: "ref.jpg", size: 10, watched: false },
        { rel: "new.fit", size: 500, watched: false },
      ],
      dirs: [],
      conflicts: [
        { rel: "ref.fit", kind: "file", src_size: 1000, src_mtime: 1760000000, dst_size: 900, dst_mtime: 1759990000, dst_ledger: "" },
        { rel: "ref.jpg", kind: "file", src_size: 10, src_mtime: 1760000000, dst_size: 12, dst_mtime: 1759990001, dst_ledger: "" },
      ],
    });
    renderCopy(plan);
    await screen.findByText("2 files already exist on Bravo");

    const group = screen.getByRole("radiogroup", { name: "ref.fit: overwrite or skip" });
    fireEvent.click(within(group).getByLabelText("Overwrite"));
    fireEvent.click(screen.getByRole("button", { name: "Copy 2 files, skip 1" }));
    await flush();

    const run = calls.find((c) => c.url === "/api/files/copy")!;
    expect(run.body).toEqual({
      from: { rig: "a", share: "EMMC Images", dir: "Preview" },
      to: { rig: "b", share: "EMMC Images", dir: "Preview" },
      dirs: [],
      items: [
        { rel: "ref.fit", size: 1000 },
        { rel: "new.fit", size: 500 },
      ],
      overwrite: [{ rel: "ref.fit", dst_size: 900, dst_mtime: 1759990000 }],
    });
  });

  it("Overwrite all, then Skip all, round-trips every row", async () => {
    const plan = copyPlan({
      conflicts: [
        { rel: "ref.fit", kind: "file", src_size: 1000, src_mtime: 1, dst_size: 1, dst_mtime: 2, dst_ledger: "" },
      ],
    });
    renderCopy(plan);
    await screen.findByText("1 file already exists on Bravo");
    fireEvent.click(screen.getByRole("button", { name: "Overwrite all" }));
    expect(screen.getByRole("button", { name: "Copy 4 files" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Skip all" }));
    expect(screen.getByRole("button", { name: "Copy 3 files, skip 1" })).toBeTruthy();
  });

  it("shows no archiving wording for a copy into a folder Commander does not watch", async () => {
    const plan = copyPlan({
      conflicts: [
        { rel: "ref.fit", kind: "file", src_size: 1000, src_mtime: 1, dst_size: 1, dst_mtime: 2, dst_ledger: "" },
      ],
      watched: { frames: 0, previews: 0, keeps_originals: false },
    });
    renderCopy(plan);
    await screen.findByText("1 file already exists on Bravo");
    expect(dialog().textContent).not.toMatch(/archiv/i);
    expect(dialog().textContent).not.toMatch(/grace period|remove them/i);
  });

  it("warns, with the counts, when files land in a watched folder", async () => {
    renderCopy(copyPlan({ watched: { frames: 3, previews: 1, keeps_originals: false } }));
    await screen.findByText(/3 frames will be archived as Bravo's frames/);
    expect(screen.getByText(/1 preview \(jpg and other excluded file types\) is not archived/)).toBeTruthy();
  });

  it("ignores Esc and the scrim during Progress; Stop is the only way out", async () => {
    const { onClose } = renderCopy(copyPlan());
    fireEvent.click(await screen.findByRole("button", { name: "Copy 4 files" }));
    await flush();
    expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();

    fireEvent.keyDown(document, { key: "Escape" });
    fireEvent.mouseDown(document.querySelector(".aac-scrim")!);
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    await flush();

    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
    expect(streams[0]).toBeTruthy();
  });

  it("keeps focus inside the dialog through checking, plan, progress and report", async () => {
    let resolvePlan!: (r: Response) => void;
    route("POST", "/api/files/copy/plan", () => new Promise<Response>((r) => (resolvePlan = r)));
    streamRoute("/api/files/copy");
    withToasts(
      <CopyDialog source={{ kind: "copy", from, mirror: false }} to={to} onClose={() => {}} onRunning={() => {}} onRelist={() => {}} />,
    );
    await flush();
    expect(screen.getByText(/Checking 2 selected items on Alpha/)).toBeTruthy();
    expect(focusInDialog()).toBe(true);

    await act(async () => resolvePlan(json(200, copyPlan())));
    await screen.findByRole("button", { name: "Copy 4 files" });
    expect(focusInDialog()).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: "Copy 4 files" }));
    await flush();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Stop" }));

    await act(async () => {
      streams[0]!.push({ type: "progress", done: 0, total: 4, bytes_done: 0, bytes_total: 4000, current: "ref.fit" });
      streams[0]!.push({ type: "item", rel: "ref.fit", outcome: "failed", code: "denied", message: "Bravo: access denied" });
      streams[0]!.push({ type: "summary", done: 3, skipped: 0, failed: 1, aborted: "" });
      streams[0]!.end();
    });
    await screen.findByRole("heading", { name: "Copied 3 of 4 files to Bravo" });
    expect(focusInDialog()).toBe(true);
    expect(document.activeElement).toBe(screen.getByRole("heading", { name: "Copied 3 of 4 files to Bravo" }));

    // A row action and its outcome keep focus in the dialog too.
    fireEvent.click(screen.getByRole("button", { name: "Retry ref.fit" }));
    await flush();
    await act(async () => {
      streams[1]!.push({ type: "summary", done: 1, skipped: 0, failed: 0, aborted: "" });
      streams[1]!.end();
    });
    await flush();
    expect(focusInDialog()).toBe(true);
  });

  it("keeps focus in the dialog when the plan fails", async () => {
    route("POST", "/api/files/copy/plan", () =>
      json(409, { error: "Conflict", code: "busy", message: "Copy on Alpha is running since 10:02." }),
    );
    withToasts(
      <CopyDialog source={{ kind: "copy", from, mirror: false }} to={to} onClose={() => {}} onRunning={() => {}} onRelist={() => {}} />,
    );
    await screen.findByText(/Copy on Alpha is running/);
    expect(focusInDialog()).toBe(true);
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
  });

  it("builds the report itself when the stream ends without a summary", async () => {
    const drive = async () => {
      fireEvent.click(await screen.findByRole("button", { name: "Copy 4 files" }));
      await flush();
      const s = streams[streams.length - 1]!;
      await act(async () => {
        s.push({ type: "progress", done: 0, total: 4, bytes_done: 0, bytes_total: 4000, current: "ref.fit" });
        // ref.fit finished with no line: it counts as done.
        s.push({ type: "progress", done: 1, total: 4, bytes_done: 1000, bytes_total: 4000, current: "M42/a.fit" });
        s.push({ type: "item", rel: "M42/a.fit", outcome: "skipped", code: "appeared", dst_size: 5, dst_mtime: 6 });
        s.push({ type: "progress", done: 2, total: 4, bytes_done: 2000, bytes_total: 4000, current: "M42/b.fit" });
      });
      await flush();
      return s;
    };

    // Stop: the user pressed it.
    renderCopy(copyPlan());
    await drive();
    fireEvent.click(screen.getByRole("button", { name: "Stop" }));
    await screen.findByRole("heading", { name: "Copy stopped" });
    const stoppedTable = screen.getByRole("table");
    expect(within(stoppedTable).queryByText("ref.fit")).toBeNull();
    expect(within(stoppedTable).getByText(/Appeared on Bravo after the check/)).toBeTruthy();
    expect(
      within(stoppedTable).getByText(
        "Stopped while copying. The destination holds either the complete file or nothing under its name; Retry copies it again.",
      ),
    ).toBeTruthy();
    expect(within(stoppedTable).getByText("Not attempted: the operation was stopped.")).toBeTruthy();
    expect(screen.getByText("1 copied")).toBeTruthy();
    cleanup();
    streams = [];

    // Interrupted: the stream just ended.
    renderCopy(copyPlan());
    const s = await drive();
    await act(async () => s.end());
    await screen.findByRole("heading", {
      name: "Copy interrupted — lost connection to Commander",
    });
    expect(screen.getByText("Not attempted: the connection to Commander was lost.")).toBeTruthy();
    expect(screen.getByText(/^Stopped while copying/)).toBeTruthy();
    cleanup();

    // The delete variant words its in-flight item for a delete.
    renderDelete(deletePlan({ unarchived: 0 }));
    streamRoute("/api/rigs/a/files/delete");
    fireEvent.click(await screen.findByRole("button", { name: "Delete 3 files and 1 folder" }));
    await flush();
    const d = streams[streams.length - 1]!;
    await act(async () => {
      d.push({ type: "progress", done: 0, total: 4, bytes_done: 0, bytes_total: 0, current: "M42/a.fit" });
      d.push({ type: "progress", done: 1, total: 4, bytes_done: 0, bytes_total: 0, current: "M42/b.fit" });
    });
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Stop" }));
    await screen.findByRole("heading", { name: "Delete stopped" });
    expect(
      screen.getByText("Stopped while deleting; it may already be gone. The pane shows its current state."),
    ).toBeTruthy();
    expect(screen.getAllByText("Not attempted: the operation was stopped.")).toHaveLength(2);
  });

  it("closes with a toast when everything copied cleanly", async () => {
    const { onClose, onRelist } = renderCopy(copyPlan());
    fireEvent.click(await screen.findByRole("button", { name: "Copy 4 files" }));
    await flush();
    await act(async () => {
      streams[0]!.push({ type: "summary", done: 4, skipped: 0, failed: 0, aborted: "" });
      streams[0]!.end();
    });
    await flush();
    expect(onClose).toHaveBeenCalled();
    expect(onRelist).toHaveBeenCalled();
    expect(screen.getByText("Copied 4 files (3.9 KB) to Bravo")).toBeTruthy();
  });

  it("names the folders a folders-only copy created in its toast", async () => {
    const { onClose } = renderCopy(copyPlan({ files: 0, bytes: 0, dirs: ["Empty", "Flats"], items: [] }));
    fireEvent.click(await screen.findByRole("button", { name: "Create 2 folders" }));
    await flush();
    await act(async () => {
      streams[0]!.push({ type: "summary", done: 0, skipped: 0, failed: 0, aborted: "" });
      streams[0]!.end();
    });
    await flush();
    expect(onClose).toHaveBeenCalled();
    expect(screen.getByText("Created 2 folders on Bravo")).toBeTruthy();
  });

  it("reports a folder it could not create rather than toasting success", async () => {
    const { onClose } = renderCopy(copyPlan({ files: 0, bytes: 0, dirs: ["Empty"], items: [] }));
    fireEvent.click(await screen.findByRole("button", { name: "Create 1 folder" }));
    await flush();
    await act(async () => {
      streams[0]!.push({
        type: "item",
        rel: "Empty",
        outcome: "failed",
        code: "denied",
        message: "Bravo refused Preview/Empty: access denied.",
      });
      streams[0]!.push({ type: "summary", done: 0, skipped: 0, failed: 1, aborted: "" });
      streams[0]!.end();
    });
    await screen.findByRole("heading", { name: "Created 0 of 1 folder on Bravo" });
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.queryByText(/^Created 1 folder on Bravo$/)).toBeNull();
    expect(screen.getByText("Empty")).toBeTruthy();
    expect(screen.getByText("Bravo refused: access denied.")).toBeTruthy();
    // Retry would send the folder as a file: the report offers none.
    expect(screen.queryByRole("button", { name: /Retry/ })).toBeNull();
  });

  it("titles a run the server ended on a wrong device and words its not-attempted rows", async () => {
    renderCopy(copyPlan());
    fireEvent.click(await screen.findByRole("button", { name: "Copy 4 files" }));
    await flush();
    await act(async () => {
      streams[0]!.push({
        type: "item",
        rel: "M42/a.fit",
        outcome: "failed",
        code: "identity",
        message: "The device at 10.0.0.2 is not Bravo. Commander will not touch it.",
      });
      for (const rel of ["M42/b.fit", "M42/c.fit"]) {
        streams[0]!.push({ type: "item", rel, outcome: "failed", code: "not_attempted" });
      }
      streams[0]!.push({ type: "summary", done: 1, skipped: 0, failed: 3, aborted: "identity" });
      streams[0]!.end();
    });
    await screen.findByRole("heading", {
      name: "Copied 1 of 4 files to Bravo — Commander could not confirm it was talking to Bravo",
    });
    expect(
      screen.getAllByText("Not attempted: Commander could not confirm it was talking to Bravo."),
    ).toHaveLength(2);
  });

  it("reports a copied file whose timestamp was not preserved, with no action", async () => {
    renderCopy(copyPlan());
    fireEvent.click(await screen.findByRole("button", { name: "Copy 4 files" }));
    await flush();
    await act(async () => {
      streams[0]!.push({ type: "item", rel: "ref.fit", outcome: "copied", code: "mtime_not_preserved" });
      streams[0]!.push({ type: "summary", done: 4, skipped: 0, failed: 0, aborted: "" });
      streams[0]!.end();
    });
    await screen.findByRole("heading", { name: "Copied 4 of 4 files to Bravo" });
    expect(screen.getByText("Copied; timestamp not preserved, so it shows the copy time.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Retry|Overwrite/ })).toBeNull();
  });

  it("asks before overwriting an unarchived file the user skipped, then runs one item", async () => {
    const plan = copyPlan({
      conflicts: [
        { rel: "ref.fit", kind: "file", src_size: 1000, src_mtime: 1, dst_size: 900, dst_mtime: 2, dst_ledger: "unarchived" },
      ],
    });
    renderCopy(plan);
    fireEvent.click(await screen.findByRole("button", { name: "Copy 3 files, skip 1" }));
    await flush();
    await act(async () => {
      streams[0]!.push({ type: "summary", done: 3, skipped: 0, failed: 0, aborted: "" });
      streams[0]!.end();
    });
    await screen.findByText("Already exists: you chose Skip.");
    fireEvent.click(screen.getByRole("button", { name: "Overwrite ref.fit" }));
    expect(screen.getByText("Overwrite ref.fit on Bravo? It has no archived copy.")).toBeTruthy();
    expect(calls.filter((c) => c.url === "/api/files/copy")).toHaveLength(1);

    fireEvent.click(screen.getByRole("button", { name: "Overwrite" }));
    await flush();
    const retry = calls.filter((c) => c.url === "/api/files/copy")[1]!;
    expect(retry.body).toMatchObject({
      items: [{ rel: "ref.fit", size: 1000 }],
      overwrite: [{ rel: "ref.fit", dst_size: 900, dst_mtime: 2 }],
    });
  });

  it("says it is waiting after 5 s without a line, naming both rigs, and clears on the next", async () => {
    renderCopy(copyPlan());
    const copy = await screen.findByRole("button", { name: "Copy 4 files" });
    vi.useFakeTimers();
    fireEvent.click(copy);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.queryByText(/Waiting for/)).toBeNull();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(4999);
    });
    expect(screen.queryByText(/Waiting for/)).toBeNull();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(screen.getByText("Waiting for Alpha and Bravo to respond…")).toBeTruthy();

    await act(async () => {
      streams[0]!.push({ type: "progress", done: 0, total: 4, bytes_done: 10, bytes_total: 4000, current: "ref.fit" });
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.queryByText(/Waiting for/)).toBeNull();
  });

  it("names one rig while waiting on a same-rig copy", async () => {
    renderCopy(copyPlan(), { to: { ...to, rig: "a", rigName: "Alpha", dir: "Other" } });
    const copy = await screen.findByRole("button", { name: "Copy 4 files" });
    vi.useFakeTimers();
    fireEvent.click(copy);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(screen.getByText("Waiting for Alpha to respond…")).toBeTruthy();
  });
});

/* ---- delete --------------------------------------------------------------- */

describe("DeleteDialog", () => {
  it("keeps Delete disabled until the unarchived checkbox is ticked", async () => {
    renderDelete(deletePlan());
    const del = await screen.findByRole("button", { name: "Delete 3 files and 1 folder" });
    expect(screen.getByText(
      "3 files have no archived copy in your library. Deleting them removes the only copy.",
    )).toBeTruthy();
    expect((del as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByLabelText("I understand these 3 files will be lost"));
    expect((del as HTMLButtonElement).disabled).toBe(false);
  });

  it("needs no checkbox when nothing is unarchived", async () => {
    renderDelete(deletePlan({ unarchived: 0 }));
    const del = await screen.findByRole("button", { name: "Delete 3 files and 1 folder" });
    expect((del as HTMLButtonElement).disabled).toBe(false);
    expect(screen.getByText("Every frame in watched folders is archived in your library.")).toBeTruthy();
  });

  it("returns to the confirmation, unticked, with the new count on 409 unarchived", async () => {
    renderDelete(deletePlan());
    let attempt = 0;
    route("POST", "/api/rigs/a/files/delete", (_url, init) => {
      attempt++;
      if (attempt === 1) {
        return json(409, {
          error: "Conflict",
          code: "unarchived",
          message: "40 files have no archived copy.",
          unarchived: 40,
        });
      }
      const s = new Stream(init.signal);
      streams.push(s);
      return s.response;
    });
    fireEvent.click(await screen.findByLabelText("I understand these 3 files will be lost"));
    fireEvent.click(screen.getByRole("button", { name: "Delete 3 files and 1 folder" }));

    await screen.findByText(
      "Since you confirmed, more files lost their archived copy: now 40, was 3. Nothing was deleted.",
    );
    const box = screen.getByLabelText("I understand these 40 files will be lost") as HTMLInputElement;
    expect(box.checked).toBe(false);
    const del = screen.getByRole("button", { name: "Delete 3 files and 1 folder" }) as HTMLButtonElement;
    expect(del.disabled).toBe(true);
    expect(focusInDialog()).toBe(true);

    fireEvent.click(box);
    fireEvent.click(del);
    await flush();
    const runs = calls.filter((c) => c.url === "/api/rigs/a/files/delete");
    expect(runs[0]!.body).toMatchObject({ acknowledged_unarchived: 3 });
    expect(runs[1]!.body).toEqual({
      share: "EMMC Images",
      dir: "Preview",
      items: deletePlan().items,
      dirs: ["M42"],
      acknowledged_unarchived: 40,
    });
  });

  it("warns when the selection is or holds a watched root", async () => {
    renderDelete(deletePlan({ unarchived: 0, watched_roots: ["Autorun"], unwatched_files: 2 }));
    await screen.findByText(/Autorun is a folder Commander watches/);
    expect(screen.getByText(/2 files are outside folders Commander watches/)).toBeTruthy();
  });
});

/* ---- the panel ------------------------------------------------------------ */

function status(id: string, name: string, over: Partial<RigStatus> = {}): RigStatus {
  return {
    id,
    name,
    state: "idle",
    online: true,
    paused: false,
    requested: "running",
    frames_tonight: 0,
    shares: [],
    device_frames: null,
    keep_originals: false,
    safe_to_power_off: "safe",
    listing_ts: 0,
    ...over,
  };
}

describe("DeviceFilesPanel", () => {
  it("defaults Left and Right to the first two online rigs and lists their shares", async () => {
    const shares = {
      share: "",
      dir: "",
      watched: false,
      writable: true,
      entries: [
        { name: "EMMC Images", dir: true, size: 0, mtime: 0, watched: false },
        { name: "Udisk Images", dir: true, size: 0, mtime: 0, watched: false },
      ],
    };
    route("GET", /^\/api\/rigs\/[bc]\/files$/, () => json(200, shares));
    const statuses = [
      status("a", "Alpha", { online: false }),
      status("b", "Bravo", { remote_name: "Cabin" }),
      status("c", "Charlie"),
    ];
    const router = createMemoryRouter(
      [{ path: "/tools", element: <DeviceFilesPanel statuses={statuses} profiles={[]} /> }],
      { initialEntries: ["/tools"] },
    );
    render(
      <ToastProvider>
        <RouterProvider router={router} />
      </ToastProvider>,
    );

    await waitFor(() => expect(router.state.location.search).toBe("?lrig=b&rrig=c"));
    const left = screen.getByRole("combobox", { name: "Left pane rig" }) as HTMLSelectElement;
    expect(left.value).toBe("b");
    for (const o of screen.getAllByRole("option", { name: "Alpha (offline)" })) {
      expect(o).toHaveProperty("disabled", true);
    }
    expect(screen.getAllByRole("option", { name: "Bravo (via Cabin)" })).toHaveLength(2);
    expect(await screen.findAllByRole("button", { name: "EMMC Images" })).toHaveLength(2);
    // Shares have no checkboxes: they can't be selected.
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(screen.getByRole("button", { name: "Copy to Charlie" })).toHaveProperty("disabled", true);

    // Opening a share is mirrored in the query string.
    route("GET", "/api/rigs/b/files", (url) =>
      url.includes("share=")
        ? json(200, {
            share: "EMMC Images",
            dir: "",
            watched: false,
            writable: true,
            entries: [{ name: "ref.fit", dir: false, size: 1024, mtime: 1760000000, watched: false }],
          })
        : json(200, shares),
    );
    routes.unshift(routes.pop()!);
    fireEvent.click(screen.getAllByRole("button", { name: "EMMC Images" })[0]!);
    await screen.findByRole("checkbox", { name: "Select ref.fit (Bravo)" });
    expect(router.state.location.search).toBe("?lrig=b&rrig=c&lshare=EMMC+Images");
    fireEvent.click(screen.getByRole("checkbox", { name: "Select ref.fit (Bravo)" }));
    expect(screen.getByText("1 selected · 1.0 KB")).toBeTruthy();
  });
});

/* ---- report row actions --------------------------------------------------- */

const live = () => dialog().querySelector("p.sr-only[aria-live]")!;

/** A copy whose report lists three denied files, ready for row actions. */
async function copyReportWithThreeFailures() {
  const r = renderCopy(copyPlan());
  fireEvent.click(await screen.findByRole("button", { name: "Copy 4 files" }));
  await flush();
  await act(async () => {
    for (const rel of ["M42/a.fit", "M42/b.fit", "M42/c.fit"]) {
      streams[0]!.push({ type: "item", rel, outcome: "failed", code: "denied", message: "Bravo: access denied" });
    }
    streams[0]!.push({ type: "summary", done: 1, skipped: 0, failed: 3, aborted: "" });
    streams[0]!.end();
  });
  await screen.findByRole("heading", { name: "Copied 1 of 4 files to Bravo" });
  return r;
}

describe("report row actions", () => {
  it("behave like Progress: Esc, the scrim and X do nothing; Stop is the way out", async () => {
    const { onClose } = await copyReportWithThreeFailures();
    fireEvent.click(screen.getByRole("button", { name: "Retry M42/a.fit" }));
    await flush();
    const stopBtn = screen.getByRole("button", { name: "Stop" });
    expect(document.activeElement).toBe(stopBtn);

    fireEvent.keyDown(document, { key: "Escape" });
    fireEvent.mouseDown(document.querySelector(".aac-scrim")!);
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    await flush();
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByRole("heading", { name: "Copied 1 of 4 files to Bravo" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
  });

  it("shows a multi-row retry's progress, updates rows as lines arrive, and Stop rebuilds the report", async () => {
    await copyReportWithThreeFailures();
    fireEvent.click(screen.getByRole("button", { name: "Retry 3 failed and not attempted" }));
    await flush();
    const run = streams[1]!;
    await act(async () => {
      run.push({ type: "progress", done: 0, total: 3, bytes_done: 0, bytes_total: 3000, current: "M42/a.fit" });
      run.push({ type: "progress", done: 1, total: 3, bytes_done: 1000, bytes_total: 3000, current: "M42/b.fit" });
    });
    await flush();
    // The counter and the current file, as in Progress.
    expect(screen.getByText("File 2 of 3")).toBeTruthy();
    expect(screen.getByText("M42/b.fit", { selector: "p" })).toBeTruthy();
    // a.fit finished cleanly before b.fit started: its row already says so.
    const table = screen.getByRole("table");
    const rowOf = (rel: string) => within(table).getByText(rel).closest("tr")!;
    expect(within(rowOf("M42/a.fit")).getByText("Copied")).toBeTruthy();
    expect(within(rowOf("M42/b.fit")).getByText("Copying…")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Stop" }));
    await screen.findByRole("heading", { name: "Copy stopped" });
    expect(within(rowOf("M42/b.fit")).getByText("Stopped")).toBeTruthy();
    expect(within(rowOf("M42/c.fit")).getByText("Not attempted")).toBeTruthy();
    expect(within(rowOf("M42/c.fit")).getByText("Not attempted: the operation was stopped.")).toBeTruthy();
    // Stopped and not attempted are not failures.
    expect(screen.getByText("2 copied")).toBeTruthy();
    expect(screen.getByText("0 failed")).toBeTruthy();
    expect(screen.getByText("2 not attempted")).toBeTruthy();
    // The live region says what happened and ends with the heading it shows;
    // focus stays in the dialog, on Close. Both follow the render that shows the
    // heading (an effect announces what was rendered), so wait for them.
    await waitFor(() => expect(live().textContent).toBe("Retried 3: 1 copied, 2 not attempted. Copy stopped"));
    expect(document.activeElement).toBe(dialog().querySelector("footer button"));
    expect(document.activeElement?.textContent).toBe("Close");
  });

  it("announces a single row's outcome with the heading it produced, never a stale one", async () => {
    await copyReportWithThreeFailures();
    // As the report opens, focus reads the heading; the live region repeats nothing.
    expect(live().textContent).toBe("");
    fireEvent.click(screen.getByRole("button", { name: "Retry M42/a.fit" }));
    await flush();
    await act(async () => {
      streams[1]!.push({ type: "summary", done: 1, skipped: 0, failed: 0, aborted: "" });
      streams[1]!.end();
    });
    await screen.findByRole("heading", { name: "Copied 2 of 4 files to Bravo" });
    await waitFor(() => expect(live().textContent).toBe("a.fit: Copied. Copied 2 of 4 files to Bravo"));
    expect(focusInDialog()).toBe(true);
  });

  it("focuses the inline confirmation, which carries its question, and returns to Overwrite on Cancel", async () => {
    const plan = copyPlan({
      conflicts: [
        { rel: "ref.fit", kind: "file", src_size: 1000, src_mtime: 1, dst_size: 900, dst_mtime: 2, dst_ledger: "unarchived" },
      ],
    });
    renderCopy(plan);
    fireEvent.click(await screen.findByRole("button", { name: "Copy 3 files, skip 1" }));
    await flush();
    await act(async () => {
      streams[0]!.push({ type: "summary", done: 3, skipped: 0, failed: 0, aborted: "" });
      streams[0]!.end();
    });
    await screen.findByText("Already exists: you chose Skip.");
    fireEvent.click(screen.getByRole("button", { name: "Overwrite ref.fit" }));
    const confirm = screen.getByRole("button", { name: "Overwrite" });
    expect(document.activeElement).toBe(confirm);
    const q = document.getElementById(confirm.getAttribute("aria-describedby")!)!;
    expect(q.textContent).toBe("Overwrite ref.fit on Bravo? It has no archived copy.");

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Overwrite ref.fit" }));
  });

  it("keeps a delete's row retry in the dialog and counts its pills like the server", async () => {
    renderDelete(deletePlan({ unarchived: 0 }));
    streamRoute("/api/rigs/a/files/delete");
    fireEvent.click(await screen.findByRole("button", { name: "Delete 3 files and 1 folder" }));
    await flush();
    await act(async () => {
      streams[0]!.push({ type: "item", rel: "M42/a.fit", outcome: "failed", code: "unreachable", message: "Alpha is unreachable" });
      streams[0]!.push({ type: "item", rel: "M42/b.fit", outcome: "failed", code: "not_attempted" });
      streams[0]!.push({ type: "item", rel: "M42/c.fit", outcome: "failed", code: "not_attempted" });
      streams[0]!.push({ type: "item", rel: "M42", outcome: "failed", code: "not_attempted" });
      streams[0]!.push({ type: "summary", done: 0, skipped: 0, failed: 4, aborted: "unreachable" });
      streams[0]!.end();
    });
    await screen.findByRole("heading", { name: "Deleted 0 of 4 items from Alpha — Alpha became unreachable" });
    // 1 failed + 3 not attempted = the server's 4 failed.
    expect(screen.getByText("1 failed")).toBeTruthy();
    expect(screen.getByText("3 not attempted")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Retry 4 failed and not attempted" }));
    await flush();
    expect(screen.getByText("Deleted 0 of 4 items")).toBeTruthy();
    fireEvent.keyDown(document, { key: "Escape" });
    await flush();
    expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
    await act(async () => {
      streams[1]!.push({ type: "summary", done: 4, skipped: 0, failed: 0, aborted: "" });
      streams[1]!.end();
    });
    await screen.findByRole("heading", { name: "Deleted 4 of 4 items from Alpha" });
    await waitFor(() => expect(live().textContent).toBe("Retried 4: 4 deleted. Deleted 4 of 4 items from Alpha"));
  });
});

/* ---- plan announcements ---------------------------------------------------- */

describe("plan accessibility", () => {
  it("describes an Overwrite choice by the cost the hint names", async () => {
    renderCopy(
      copyPlan({
        conflicts: [
          { rel: "ref.fit", kind: "file", src_size: 1000, src_mtime: 1, dst_size: 900, dst_mtime: 2, dst_ledger: "unarchived" },
        ],
      }),
    );
    const group = await screen.findByRole("radiogroup", { name: "ref.fit: overwrite or skip" });
    const overwrite = within(group).getByLabelText("Overwrite");
    const hint = document.getElementById(overwrite.getAttribute("aria-describedby")!)!;
    expect(hint.textContent).toBe("Not archived yet: overwriting loses it.");
    expect(within(group).getByLabelText("Skip").getAttribute("aria-describedby")).toBeNull();
  });

  it("alerts a failed plan", async () => {
    route("POST", "/api/files/copy/plan", () =>
      json(409, { error: "Conflict", code: "busy", message: "Copy on Alpha is running since 10:02." }),
    );
    withToasts(
      <CopyDialog source={{ kind: "copy", from, mirror: false }} to={to} onClose={() => {}} onRunning={() => {}} onRelist={() => {}} />,
    );
    expect((await screen.findByRole("alert")).textContent).toMatch(/Copy on Alpha is running/);
  });

  it("reads the new totals out once the user pauses after a decision", async () => {
    renderCopy(
      copyPlan({
        conflicts: [
          { rel: "ref.fit", kind: "file", src_size: 1000, src_mtime: 1, dst_size: 1, dst_mtime: 2, dst_ledger: "" },
        ],
      }),
    );
    await screen.findByText("1 file already exists on Bravo");
    expect(live().textContent).toBe("");
    vi.useFakeTimers();
    fireEvent.click(screen.getByRole("button", { name: "Overwrite all" }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(599);
    });
    expect(live().textContent).toBe("");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(live().textContent).toBe("Copy 4 files (3.9 KB) to Bravo. Copy 4 files.");
  });

  it("puts the checking line in a live region", async () => {
    route("POST", "/api/files/copy/plan", () => new Promise<Response>(() => {}));
    withToasts(
      <CopyDialog source={{ kind: "copy", from, mirror: false }} to={to} onClose={() => {}} onRunning={() => {}} onRelist={() => {}} />,
    );
    await flush();
    const text = screen.getByText("Checking 2 selected items on Alpha…");
    expect(text.getAttribute("aria-live")).toBe("polite");
  });

  it("says why Delete is disabled until the box is ticked, and words its toast like the button", async () => {
    renderDelete(deletePlan());
    const del = await screen.findByRole("button", { name: "Delete 3 files and 1 folder" });
    expect(document.getElementById(del.getAttribute("aria-describedby")!)!.textContent).toBe(
      "Tick the box above to enable",
    );
    fireEvent.click(screen.getByLabelText("I understand these 3 files will be lost"));
    expect(del.getAttribute("aria-describedby")).toBeNull();

    streamRoute("/api/rigs/a/files/delete");
    fireEvent.click(del);
    await flush();
    await act(async () => {
      streams[0]!.push({ type: "summary", done: 4, skipped: 0, failed: 0, aborted: "" });
      streams[0]!.end();
    });
    await flush();
    expect(screen.getByText("Deleted 3 files and 1 folder from Alpha")).toBeTruthy();
  });
});

/* ---- panel focus, labels and URL states ----------------------------------- */

const shareList = {
  share: "",
  dir: "",
  watched: false,
  writable: true,
  entries: [{ name: "EMMC Images", dir: true, size: 0, mtime: 0, watched: false }],
};
const folder = (entries: { name: string; dir?: boolean }[]) => ({
  share: "EMMC Images",
  dir: "",
  watched: false,
  writable: true,
  entries: entries.map((e) => ({ name: e.name, dir: !!e.dir, size: e.dir ? 0 : 1024, mtime: 1760000000, watched: false })),
});

function renderPanel(url: string, profiles = [{ id: "b", name: "Bravo", managed: true }, { id: "c", name: "Charlie", managed: true }]) {
  const statuses = [status("b", "Bravo"), status("c", "Charlie")];
  const router = createMemoryRouter(
    [
      {
        path: "/tools",
        element: <DeviceFilesPanel statuses={statuses} profiles={profiles as never} />,
      },
    ],
    { initialEntries: [url] },
  );
  render(
    <ToastProvider>
      <RouterProvider router={router} />
    </ToastProvider>,
  );
  return router;
}

describe("DeviceFilesPanel accessibility", () => {
  it("names each pane's listing region, and a file's name toggles its selection", async () => {
    route("GET", "/api/rigs/b/files", (url) =>
      url.includes("share=") ? json(200, folder([{ name: "ref.fit" }, { name: "M42", dir: true }])) : json(200, shareList),
    );
    route("GET", "/api/rigs/c/files", () => json(200, shareList));
    renderPanel("/tools?lrig=b&rrig=c&lshare=EMMC+Images");
    const box = (await screen.findByRole("checkbox", { name: "Select ref.fit (Bravo)" })) as HTMLInputElement;
    expect(screen.getByRole("region", { name: "Left pane: Bravo › EMMC Images" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "Right pane: Charlie" })).toBeTruthy();

    fireEvent.click(screen.getByText("ref.fit"));
    expect(box.checked).toBe(true);
    const count = screen.getByText("1 selected · 1.0 KB");
    expect(count.getAttribute("aria-live")).toBe("polite");
    // A folder's name still opens it.
    expect(screen.getByRole("button", { name: "M42" })).toBeTruthy();
    // The right pane's copy button names the left pane's rig, and its arrow
    // takes the right pane's direction (left when side by side, up when stacked).
    const svg = screen.getByRole("button", { name: "Copy to Bravo" }).querySelector("svg")!;
    expect(svg.getAttribute("class")).toContain("dfb-copy-icon--right");
  });

  it("asks to select first on a visible line, and keeps focus on the rig selector", async () => {
    route("GET", "/api/rigs/b/files", (url) => (url.includes("share=") ? json(200, folder([{ name: "ref.fit" }])) : json(200, shareList)));
    route("GET", "/api/rigs/c/files", () => json(200, shareList));
    renderPanel("/tools?lrig=b&rrig=c&lshare=EMMC+Images");
    await screen.findByRole("checkbox", { name: "Select ref.fit (Bravo)" });
    expect(screen.getByText("Select files or folders to download, copy, rename or delete.")).toBeTruthy();

    const select = screen.getByRole("combobox", { name: "Right pane rig" });
    select.focus();
    fireEvent.change(select, { target: { value: "b" } });
    await flush();
    expect(document.activeElement).toBe(select);
  });

  it("starts a download with a toast and focuses the pane's listing", async () => {
    route("GET", "/api/rigs/b/files", (url) =>
      url.includes("/download")
        ? json(200, { filename: "ref.fit", files: 1, bytes: 1024, zipped: false })
        : url.includes("share=")
          ? json(200, folder([{ name: "ref.fit" }]))
          : json(200, shareList),
    );
    route("GET", /^\/api\/rigs\/b\/files\/download/, () => json(200, { filename: "ref.fit", files: 1, bytes: 1024, zipped: false }));
    routes.unshift(routes.pop()!);
    route("GET", "/api/rigs/c/files", () => json(200, shareList));
    renderPanel("/tools?lrig=b&rrig=c&lshare=EMMC+Images");
    fireEvent.click(await screen.findByRole("checkbox", { name: "Select ref.fit (Bravo)" }));
    const left = screen.getByRole("group", { name: "Left pane actions" });
    fireEvent.click(within(left).getByRole("button", { name: "Download" }));
    await screen.findByText("Download started: ref.fit");
    await flush();
    expect(document.activeElement).toBe(screen.getByRole("region", { name: "Left pane: Bravo › EMMC Images" }));
  });

  it("returns focus to the source pane's listing when a dialog closes", async () => {
    route("GET", "/api/rigs/b/files", (url) => (url.includes("share=") ? json(200, folder([{ name: "ref.fit" }])) : json(200, shareList)));
    route("GET", "/api/rigs/c/files", () => json(200, shareList));
    renderPanel("/tools?lrig=b&rrig=c&lshare=EMMC+Images");
    await screen.findByRole("checkbox", { name: "Select ref.fit (Bravo)" });
    const left = screen.getByRole("group", { name: "Left pane actions" });
    fireEvent.click(within(left).getByRole("button", { name: "New folder" }));
    expect(screen.getByRole("heading", { name: "New folder in Bravo › EMMC Images" })).toBeTruthy();
    fireEvent.keyDown(document, { key: "Escape" });
    await flush();
    expect(document.activeElement).toBe(screen.getByRole("region", { name: "Left pane: Bravo › EMMC Images" }));
  });

  it("treats an unknown rig in the URL as no rig", async () => {
    route("GET", "/api/rigs/c/files", () => json(200, shareList));
    renderPanel("/tools?lrig=typo&rrig=c");
    expect(await screen.findByText("typo is not one of your managed rigs")).toBeTruthy();
    expect(calls.some((c) => c.url.startsWith("/api/rigs/typo"))).toBe(false);
    const select = screen.getByRole("combobox", { name: "Left pane rig" });
    expect(within(select).queryByRole("option", { name: "typo" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    const left = screen.getByRole("group", { name: "Left pane actions" });
    const newFolder = within(left).getByRole("button", { name: "New folder" });
    expect(newFolder).toHaveProperty("disabled", true);
    expect(newFolder.getAttribute("title")).toBe("Choose a rig first.");
    expect(screen.queryByText(/still identifying/)).toBeNull();
  });

  it("refuses a path the server rejects, with nothing to retry or create", async () => {
    route("GET", "/api/rigs/b/files", (url) =>
      url.includes("share=")
        ? json(400, { error: "Bad Request", code: "bad_path", message: 'invalid path "../x"' })
        : json(200, shareList),
    );
    route("GET", "/api/rigs/c/files", () => json(200, shareList));
    renderPanel("/tools?lrig=b&rrig=c&lshare=EMMC+Images&ldir=..%2Fx");
    expect(await screen.findByText("That path isn't valid")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    const left = screen.getByRole("group", { name: "Left pane actions" });
    expect(within(left).getByRole("button", { name: "New folder" })).toHaveProperty("disabled", true);
  });

  it("shows the server's not_found message, which names what is missing", async () => {
    route("GET", "/api/rigs/b/files", (url) =>
      url.includes("share=")
        ? json(404, { error: "Not Found", code: "not_found", message: "Nope isn't a share on Bravo" })
        : json(200, shareList),
    );
    route("GET", "/api/rigs/c/files", () => json(200, shareList));
    renderPanel("/tools?lrig=b&rrig=c&lshare=Nope");
    expect(await screen.findByText("Nope isn't a share on Bravo")).toBeTruthy();
  });
});

/* ---- upload --------------------------------------------------------------- */

/** XMLHttpRequest for the staging request: the test drives its progress and
 * its answer, or breaks the connection. */
class FakeXHR {
  static all: FakeXHR[] = [];
  method = "";
  url = "";
  headers: Record<string, string> = {};
  body: unknown = null;
  status = 0;
  statusText = "";
  responseText = "";
  upload: { onprogress: ((e: { loaded: number; total: number; lengthComputable: boolean }) => void) | null } = {
    onprogress: null,
  };
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onabort: (() => void) | null = null;
  aborted = false;
  open(method: string, url: string) {
    this.method = method;
    this.url = url;
  }
  setRequestHeader(k: string, v: string) {
    this.headers[k] = v;
  }
  send(body: unknown) {
    this.body = body;
    FakeXHR.all.push(this);
  }
  abort() {
    this.aborted = true;
    this.onabort?.();
  }
  progress(loaded: number, total: number) {
    this.upload.onprogress?.({ loaded, total, lengthComputable: true });
  }
  respond(status: number, body: unknown, statusText = "") {
    this.status = status;
    this.statusText = statusText;
    this.responseText = typeof body === "string" ? body : JSON.stringify(body);
    this.onload?.();
  }
  fail() {
    this.onerror?.();
  }
}

const upFile = () => new File([new Uint8Array(1024)], "ref.fit", { lastModified: 1_760_000_000_000 });
const stageXhrs = () => FakeXHR.all.filter((x) => x.url.startsWith("/api/files/upload/stage"));

function uploadPlan(over: Partial<UploadPlan> = {}): UploadPlan {
  return {
    files: 1,
    bytes: 1024,
    dirs: [],
    items: [{ rel: "ref.fit", size: 1024, watched: false }],
    conflicts: [],
    to_dir_missing: 0,
    watched: { frames: 0, previews: 0, keeps_originals: false },
    free_bytes: 1024 ** 3,
    ...over,
  };
}

const conflictPlan = (ledger: "" | "unarchived" | "archived" = "") =>
  uploadPlan({
    conflicts: [
      {
        rel: "ref.fit",
        kind: "file",
        src_size: 1024,
        src_mtime: 1760000000,
        dst_size: 500,
        dst_mtime: 1759000000,
        dst_ledger: ledger,
      },
    ],
  });

/** An upload dialog that unmounts when it closes, as the panel's does. */
function UploadHarness({ file, onClose }: { file: File; onClose: () => void }) {
  const [open, setOpen] = useState(true);
  if (!open) return null;
  return (
    <CopyDialog
      source={{ kind: "upload", file }}
      to={to}
      onClose={() => {
        onClose();
        setOpen(false);
      }}
      onRunning={() => {}}
      onRelist={() => {}}
    />
  );
}

/** Opens the upload dialog and completes the send; the plan answers `plan`. */
async function renderUpload(plan: UploadPlan | (() => Response), file = upFile()) {
  route("POST", "/api/files/upload/plan", () => (typeof plan === "function" ? plan() : json(200, plan)));
  streamRoute("/api/files/upload");
  route("POST", "/api/files/upload/discard", () => new Response(null, { status: 204 }));
  const onClose = vi.fn();
  withToasts(<UploadHarness file={file} onClose={onClose} />);
  await flush();
  return { onClose };
}

async function finishSend(id = "stage1", xhr = stageXhrs()[stageXhrs().length - 1]!) {
  await act(async () => xhr.respond(200, { id, size: 1024 }));
  await flush();
}

const planCalls = () => calls.filter((c) => c.url === "/api/files/upload/plan");
const runCalls = () => calls.filter((c) => c.url === "/api/files/upload");
const discardCalls = () => calls.filter((c) => c.url === "/api/files/upload/discard");

describe("upload dialog", () => {
  beforeEach(() => {
    FakeXHR.all = [];
    vi.stubGlobal("XMLHttpRequest", FakeXHR);
  });

  it("shows the browser's progress while sending, and Stop there closes with no plan request", async () => {
    const { onClose } = await renderUpload(uploadPlan());
    const xhr = stageXhrs()[0]!;
    expect(xhr.url).toBe("/api/files/upload/stage?name=ref.fit&mtime_ms=1760000000000");
    expect(xhr.body).toBeInstanceOf(File);
    expect(screen.getByRole("heading", { name: "Upload ref.fit to Bravo" })).toBeTruthy();
    expect(screen.getByText("Nothing is written to Bravo until you confirm the upload.")).toBeTruthy();
    await act(async () => xhr.progress(512, 1024));
    await flush();
    expect(screen.getByText("Sending ref.fit to Commander")).toBeTruthy();
    expect(screen.getByText(/512 B of 1\.0 KB/)).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "Sending progress" }).getAttribute("aria-valuenow")).toBe("50");
    const stop = screen.getByRole("button", { name: "Stop" });
    expect(document.activeElement).toBe(stop);

    // A stray tap on the scrim does nothing while sending.
    fireEvent.mouseDown(document.querySelector(".aac-scrim")!);
    expect(onClose).not.toHaveBeenCalled();

    fireEvent.click(stop);
    await flush();
    expect(xhr.aborted).toBe(true);
    expect(onClose).toHaveBeenCalled();
    expect(planCalls()).toHaveLength(0);
    expect(discardCalls()).toHaveLength(0);
    // The dialog is gone, so the outcome is announced politely.
    const toast = screen.getByText("Upload stopped. Nothing was written to Bravo.");
    expect(toast.closest('[aria-live="polite"]')).toBeTruthy();
  });

  it("makes the header Close and Esc while sending do exactly what Stop does", async () => {
    for (const close of [
      () => fireEvent.keyDown(document, { key: "Escape" }),
      () => fireEvent.click(within(dialog()).getAllByRole("button", { name: "Close" })[0]!),
    ]) {
      FakeXHR.all = [];
      const { onClose } = await renderUpload(uploadPlan());
      const xhr = stageXhrs()[0]!;
      close();
      await flush();
      expect(xhr.aborted).toBe(true);
      expect(onClose).toHaveBeenCalledTimes(1);
      expect(screen.getByText("Upload stopped. Nothing was written to Bravo.")).toBeTruthy();
      expect(planCalls()).toHaveLength(0);
      cleanup();
    }
  });

  it("offers Send again after a network error while sending", async () => {
    await renderUpload(uploadPlan());
    await act(async () => stageXhrs()[0]!.fail());
    await flush();
    expect(
      screen.getByText(
        "Couldn't send ref.fit to Commander: the connection was lost, or your sign-in ended. Nothing was written to Bravo.",
      ),
    ).toBeTruthy();
    expect(focusInDialog()).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Send again" }));
    await flush();
    expect(stageXhrs()).toHaveLength(2);
    await finishSend();
    expect(planCalls()).toHaveLength(1);
    expect(planCalls()[0]!.body).toEqual({ id: "stage1", to: { rig: "b", share: "EMMC Images", dir: "Preview" } });
    expect(await screen.findByRole("button", { name: "Upload" })).toBeTruthy();
  });

  it("opens Plan failed with a staging refusal's sentence, and words a proxy's answer", async () => {
    await renderUpload(uploadPlan());
    await act(async () =>
      stageXhrs()[0]!.respond(507, {
        error: "Insufficient Storage",
        code: "staging_full",
        message: "Commander is already holding 4 uploads in progress. Try again when one finishes.",
      }),
    );
    expect(
      (await screen.findByRole("alert")).textContent,
    ).toBe("Commander is already holding 4 uploads in progress. Try again when one finishes.");
    expect(planCalls()).toHaveLength(0);
    cleanup();

    FakeXHR.all = [];
    await renderUpload(uploadPlan());
    await act(async () => stageXhrs()[0]!.respond(413, "<html>413 Request Entity Too Large</html>", "Payload Too Large"));
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Commander, or a proxy in front of it, refused the upload (HTTP 413).",
    );
  });

  it("refuses a picked name the device can't hold before sending anything, with Close only", async () => {
    const long = new File(["x"], `${"x".repeat(225)}`);
    await renderUpload(uploadPlan(), long);
    expect(screen.getByRole("alert").textContent).toMatch(/name too long for Bravo\)\. Rename it on your computer/);
    expect(FakeXHR.all).toHaveLength(0);
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    expect(screen.getAllByRole("button", { name: "Close" }).length).toBeGreaterThan(0);
  });

  it("shows a conflict's Overwrite or Skip, defaulting to Skip, and sends the binding only for Overwrite", async () => {
    await renderUpload(conflictPlan());
    await finishSend();
    expect(await screen.findByText("ref.fit already exists on Bravo")).toBeTruthy();
    const group = screen.getByRole("radiogroup", { name: "ref.fit: overwrite or skip" });
    expect((within(group).getByRole("radio", { name: "Skip" }) as HTMLInputElement).checked).toBe(true);
    // Skip writes nothing, so there is nothing to start, and the plan says why.
    const upload = screen.getByRole("button", { name: "Upload" });
    expect(upload).toHaveProperty("disabled", true);
    const why = screen.getByText("Nothing to upload: you chose Skip. Choose Overwrite to replace the file on Bravo.");
    expect(upload.getAttribute("aria-describedby")).toBe(why.id);

    fireEvent.click(within(group).getByRole("radio", { name: "Overwrite" }));
    expect(screen.queryByText(/^Nothing to upload/)).toBeNull();
    expect(screen.getByRole("button", { name: "Upload and overwrite" }).getAttribute("aria-describedby")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Upload and overwrite" }));
    await flush();
    expect(runCalls()[0]!.body).toEqual({
      id: "stage1",
      to: { rig: "b", share: "EMMC Images", dir: "Preview" },
      dst_size: 500,
      dst_mtime: 1759000000,
    });
  });

  it("asks before overwriting an unarchived destination", async () => {
    await renderUpload(conflictPlan("unarchived"));
    await finishSend();
    await screen.findByText("Not archived yet: overwriting loses it.");
    fireEvent.click(screen.getByRole("radio", { name: "Overwrite" }));
    fireEvent.click(screen.getByRole("button", { name: "Upload and overwrite" }));
    await flush();
    expect(runCalls()).toHaveLength(0);
    expect(screen.getByText("Overwrite ref.fit on Bravo? It has no archived copy.")).toBeTruthy();
    const overwrites = screen.getAllByRole("button", { name: "Overwrite" });
    const confirm = overwrites[overwrites.length - 1]!;
    expect(document.activeElement).toBe(confirm);
    fireEvent.click(confirm);
    await flush();
    expect(runCalls()).toHaveLength(1);
  });

  it("sends no binding without a conflict, and closes on success with the toast and one discard", async () => {
    const { onClose } = await renderUpload(uploadPlan());
    await finishSend();
    expect(screen.getByRole("heading", { name: "Upload ref.fit (1.0 KB) to Bravo" })).toBeTruthy();
    // An upload into an unwatched folder never mentions archiving.
    expect(dialog().textContent).not.toMatch(/archiv/i);
    fireEvent.click(screen.getByRole("button", { name: "Upload" }));
    await flush();
    expect(runCalls()[0]!.body).toEqual({ id: "stage1", to: { rig: "b", share: "EMMC Images", dir: "Preview" } });
    expect(screen.getByText("Writing ref.fit to Bravo")).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "Upload progress" })).toBeTruthy();
    await act(async () => {
      streams[0]!.push({ type: "progress", done: 0, total: 1, bytes_done: 0, bytes_total: 1024, current: "ref.fit" });
      streams[0]!.push({ type: "summary", done: 1, skipped: 0, failed: 0, aborted: "" });
      streams[0]!.end();
    });
    await flush();
    expect(onClose).toHaveBeenCalled();
    expect(screen.getByText("Uploaded ref.fit (1.0 KB) to Bravo")).toBeTruthy();
    expect(discardCalls()).toEqual([{ url: "/api/files/upload/discard", method: "POST", body: { id: "stage1" } }]);
  });

  it("warns about archiving and space only where they apply", async () => {
    await renderUpload(
      uploadPlan({
        items: [{ rel: "ref.fit", size: 1024, watched: true }],
        watched: { frames: 1, previews: 0, keeps_originals: false },
        free_bytes: 100,
      }),
    );
    await finishSend();
    expect(await screen.findByText(/1 frame will be archived as one of Bravo's frames/)).toBeTruthy();
    expect(screen.getByText("This file needs 1.0 KB; EMMC Images on Bravo has 100 B free.")).toBeTruthy();
  });

  it("names a folder holding the name, with nothing to choose", async () => {
    await renderUpload(
      uploadPlan({
        conflicts: [
          { rel: "ref.fit", kind: "type", src_size: 1024, src_mtime: 1, dst_size: 0, dst_mtime: 1, dst_ledger: "" },
        ],
      }),
    );
    await finishSend();
    expect(await screen.findByText("ref.fit can't be uploaded here: a folder has that name.")).toBeTruthy();
    expect(screen.queryByRole("radiogroup")).toBeNull();
    expect(screen.getByRole("button", { name: "Upload" })).toHaveProperty("disabled", true);
  });

  it("gives Stop while writing an 'Upload stopped' result with the stopped row", async () => {
    await renderUpload(uploadPlan());
    await finishSend();
    fireEvent.click(screen.getByRole("button", { name: "Upload" }));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Stop" }));
    await screen.findByRole("heading", { name: "Upload stopped" });
    expect(screen.getByText(/^Stopped while uploading\. Bravo holds either the complete file/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry ref.fit" })).toBeTruthy();
    expect(focusInDialog()).toBe(true);
  });

  it("titles a lost connection while writing as interrupted, with the stopped row's wording", async () => {
    await renderUpload(uploadPlan());
    await finishSend();
    fireEvent.click(screen.getByRole("button", { name: "Upload" }));
    await flush();
    await act(async () => {
      streams[0]!.push({ type: "progress", done: 0, total: 1, bytes_done: 512, bytes_total: 1024, current: "ref.fit" });
      streams[0]!.end();
    });
    await screen.findByRole("heading", {
      name: "Upload interrupted — lost connection to Commander, or your sign-in ended",
    });
    expect(screen.getByText(/^Stopped while uploading\./)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry ref.fit" })).toBeTruthy();
  });

  it("retries a failed row with the same staging id and sends no file", async () => {
    await renderUpload(uploadPlan());
    await finishSend();
    fireEvent.click(screen.getByRole("button", { name: "Upload" }));
    await flush();
    await act(async () => {
      streams[0]!.push({ type: "item", rel: "ref.fit", outcome: "failed", code: "unreachable", message: "Bravo: unreachable" });
      streams[0]!.push({ type: "summary", done: 0, skipped: 0, failed: 1, aborted: "unreachable" });
      streams[0]!.end();
    });
    await screen.findByRole("heading", { name: "Couldn't upload ref.fit to Bravo" });
    expect(screen.getByText(/^Bravo is unreachable \(tried 4 times\)/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry ref.fit" }));
    await flush();
    expect(runCalls()).toHaveLength(2);
    expect((runCalls()[1]!.body as { id: string }).id).toBe("stage1");
    expect(stageXhrs()).toHaveLength(1);
    await act(async () => {
      streams[1]!.push({ type: "item", rel: "ref.fit", outcome: "failed", code: "error", message: "disk full" });
      streams[1]!.push({ type: "summary", done: 0, skipped: 0, failed: 1, aborted: "" });
      streams[1]!.end();
    });
    await flush();
    expect(screen.getByText("disk full")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry ref.fit" })).toBeTruthy();
  });

  it("overwrites an appeared file from the result with the same id, bound to what it saw", async () => {
    await renderUpload(uploadPlan());
    await finishSend();
    fireEvent.click(screen.getByRole("button", { name: "Upload" }));
    await flush();
    await act(async () => {
      streams[0]!.push({ type: "item", rel: "ref.fit", outcome: "skipped", code: "appeared", dst_size: 7, dst_mtime: 8 });
      streams[0]!.push({ type: "summary", done: 0, skipped: 1, failed: 0, aborted: "" });
      streams[0]!.end();
    });
    await screen.findByRole("heading", { name: "ref.fit was not uploaded to Bravo" });
    fireEvent.click(screen.getByRole("button", { name: "Overwrite ref.fit" }));
    fireEvent.click(await screen.findByRole("button", { name: "Overwrite" }));
    await flush();
    expect(runCalls()[1]!.body).toEqual({
      id: "stage1",
      to: { rig: "b", share: "EMMC Images", dir: "Preview" },
      dst_size: 7,
      dst_mtime: 8,
    });
    expect(stageXhrs()).toHaveLength(1);
  });

  it("returns to the plan with a busy refusal's reason, keeping the staged file", async () => {
    await renderUpload(uploadPlan());
    await finishSend();
    routes.unshift({
      match: (url, m) => m === "POST" && url === "/api/files/upload",
      handle: () => json(409, { error: "Conflict", code: "busy", message: "Copy on Alpha is running since 10:02." }),
    });
    fireEvent.click(screen.getByRole("button", { name: "Upload" }));
    await screen.findByText("Copy on Alpha is running since 10:02.");
    expect(screen.getByRole("button", { name: "Upload" })).toBeTruthy();
    expect(discardCalls()).toHaveLength(0);
  });

  it("re-stages the held File when the plan or the run finds the staged file gone, and repeats the request", async () => {
    let planAnswers = 0;
    await renderUpload(() =>
      ++planAnswers === 1
        ? json(410, { error: "Gone", code: "staging_gone", message: "That upload is no longer held by Commander." })
        : json(200, uploadPlan()),
    );
    await finishSend("old");
    // The plan said gone: the file is sent again, then the plan repeated.
    expect(stageXhrs()).toHaveLength(2);
    expect(screen.getByText("Sending ref.fit to Commander")).toBeTruthy();
    await finishSend("new");
    expect(planCalls().map((c) => (c.body as { id: string }).id)).toEqual(["old", "new"]);
    await screen.findByRole("button", { name: "Upload" });

    // The run finds it gone too.
    routes.unshift({
      match: (url, m) => m === "POST" && url === "/api/files/upload",
      handle: () => {
        routes.shift();
        return json(410, { error: "Gone", code: "staging_gone", message: "gone" });
      },
    });
    fireEvent.click(screen.getByRole("button", { name: "Upload" }));
    await flush();
    expect(stageXhrs()).toHaveLength(3);
    await finishSend("newer");
    expect(runCalls().map((c) => (c.body as { id: string }).id)).toEqual(["new", "newer"]);
    expect(screen.getByText("Writing ref.fit to Bravo")).toBeTruthy();
  });

  it("sends one discard when the dialog closes", async () => {
    const { onClose } = await renderUpload(uploadPlan());
    await finishSend();
    await screen.findByRole("button", { name: "Upload" });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await flush();
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(discardCalls()).toHaveLength(1);
    expect(discardCalls()[0]!.body).toEqual({ id: "stage1" });
  });
});

describe("copy plan refusals", () => {
  it("leads Plan failed with a reserved-path refusal's own sentence", async () => {
    const sentence =
      "Autorun would stand where EMMC Images/Autorun must go. Leave Autorun out of the selection, or rename it on Alpha first.";
    route("POST", "/api/files/copy/plan", () => json(409, { error: "Conflict", code: "watched", message: sentence }));
    withToasts(
      <CopyDialog source={{ kind: "copy", from, mirror: false }} to={to} onClose={() => {}} onRunning={() => {}} onRelist={() => {}} />,
    );
    expect((await screen.findByRole("alert")).textContent).toBe(sentence);
  });
});

/* ---- toolbar: upload and rename ------------------------------------------- */

type Entry = { name: string; dir?: boolean; rename_refused?: { rule: "watched" | "reserved" | "unchecked"; message: string } };
const listing = (dir: string, entries: Entry[], watched = false) => ({
  share: "EMMC Images",
  dir,
  watched,
  writable: true,
  entries: entries.map((e) => ({
    name: e.name,
    dir: !!e.dir,
    size: e.dir ? 0 : 1024,
    mtime: 1760000000,
    watched: false,
    ...(e.rename_refused ? { rename_refused: e.rename_refused } : {}),
  })),
});

describe("toolbar upload and rename", () => {
  const r1 = { rule: "watched" as const, message: "Commander archives this folder by name; renaming here would make it archive these frames again." };

  it("keeps the file input out of the tab order and the accessibility tree, and opens the dialog on a pick", async () => {
    FakeXHR.all = [];
    vi.stubGlobal("XMLHttpRequest", FakeXHR);
    route("GET", "/api/rigs/b/files", (url) => (url.includes("share=") ? json(200, listing("", [{ name: "ref.fit" }])) : json(200, shareList)));
    route("GET", "/api/rigs/c/files", () => json(200, shareList));
    renderPanel("/tools?lrig=b&rrig=c&lshare=EMMC+Images");
    await screen.findByRole("checkbox", { name: "Select ref.fit (Bravo)" });
    const left = screen.getByRole("group", { name: "Left pane actions" });
    const input = left.querySelector('input[type="file"]') as HTMLInputElement;
    expect(input.hidden).toBe(true);
    expect(input.tabIndex).toBe(-1);
    expect(input.multiple).toBe(false);
    // Not in the accessibility tree: no role finds it.
    expect(within(left).queryAllByRole("button").map((b) => b.textContent)).not.toContain("");
    expect(within(left).queryByLabelText(/file/i)).toBeNull();

    const click = vi.spyOn(input, "click");
    fireEvent.click(within(left).getByRole("button", { name: "Upload" }));
    expect(click).toHaveBeenCalled();
    fireEvent.change(input, { target: { files: [upFile()] } });
    await flush();
    expect(screen.getByRole("heading", { name: "Upload ref.fit to Bravo" })).toBeTruthy();
    expect(input.value).toBe("");
  });

  it("states R1 in a watched folder once in its notice, and shows other refusals on a visible line", async () => {
    route("GET", "/api/rigs/b/files", (url) =>
      url.includes("share=")
        ? json(200, listing("Autorun", [{ name: "a.fit", rename_refused: r1 }], true))
        : json(200, shareList),
    );
    route("GET", "/api/rigs/c/files", () => json(200, shareList));
    renderPanel("/tools?lrig=b&rrig=c&lshare=EMMC+Images&ldir=Autorun");
    fireEvent.click(await screen.findByRole("checkbox", { name: "Select a.fit (Bravo)" }));
    expect(screen.getByText(/Commander watches this folder\..*Items here can't be renamed\.$/)).toBeTruthy();
    const left = screen.getByRole("group", { name: "Left pane actions" });
    const rename = within(left).getByRole("button", { name: "Rename" });
    expect(rename).toHaveProperty("disabled", true);
    expect(rename.getAttribute("title")).toBe(r1.message);
    expect(document.querySelector(".dfb-reason")?.textContent ?? "").not.toBe(r1.message);
    expect(screen.queryAllByText(r1.message, { selector: "p" })).toHaveLength(0);
  });

  it("returns focus to Upload however the upload dialog closes", async () => {
    FakeXHR.all = [];
    vi.stubGlobal("XMLHttpRequest", FakeXHR);
    route("GET", "/api/rigs/b/files", (url) => (url.includes("share=") ? json(200, listing("", [{ name: "x.fit" }])) : json(200, shareList)));
    route("GET", "/api/rigs/c/files", () => json(200, shareList));
    route("POST", "/api/files/upload/plan", () => json(200, uploadPlan()));
    streamRoute("/api/files/upload");
    route("POST", "/api/files/upload/discard", () => new Response(null, { status: 204 }));
    renderPanel("/tools?lrig=b&rrig=c&lshare=EMMC+Images");
    await screen.findByRole("checkbox", { name: "Select x.fit (Bravo)" });
    const left = screen.getByRole("group", { name: "Left pane actions" });
    const input = left.querySelector('input[type="file"]') as HTMLInputElement;
    const upload = within(left).getByRole("button", { name: "Upload" });
    const pick = async () => {
      upload.focus();
      fireEvent.change(input, { target: { files: [upFile()] } });
      await flush();
      expect(screen.getByRole("dialog")).toBeTruthy();
    };

    // Esc while sending (Stop).
    await pick();
    fireEvent.keyDown(document, { key: "Escape" });
    await flush();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(upload);

    // Cancel on the Plan.
    await pick();
    await finishSend();
    fireEvent.click(await within(dialog()).findByRole("button", { name: "Cancel" }));
    await flush();
    expect(document.activeElement).toBe(upload);

    // Success.
    await pick();
    await finishSend();
    fireEvent.click(await within(dialog()).findByRole("button", { name: "Upload" }));
    await flush();
    await act(async () => {
      streams[streams.length - 1]!.push({ type: "summary", done: 1, skipped: 0, failed: 0, aborted: "" });
      streams[streams.length - 1]!.end();
    });
    await flush();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(upload);
  });

  it("announces a single selection's rename refusal through the pane's live region", async () => {
    const r2 = { rule: "reserved" as const, message: "Autorun is, or holds, EMMC Images/Autorun, a folder Commander depends on." };
    route("GET", "/api/rigs/b/files", (url) =>
      url.includes("share=")
        ? json(200, listing("", [{ name: "Autorun", dir: true, rename_refused: r2 }, { name: "x.fit" }]))
        : json(200, shareList),
    );
    route("GET", "/api/rigs/c/files", () => json(200, shareList));
    renderPanel("/tools?lrig=b&rrig=c&lshare=EMMC+Images");
    const box = await screen.findByRole("checkbox", { name: "Select Autorun (Bravo)" });
    const left = screen.getByRole("group", { name: "Left pane actions" });
    const live = left.querySelector('[aria-live="polite"]')!;
    fireEvent.click(box);
    expect(live.textContent).toBe(`1 selected. ${r2.message}`);
    // Two selected: Rename is out for another reason, and nothing more is said.
    fireEvent.click(screen.getByRole("checkbox", { name: "Select x.fit (Bravo)" }));
    expect(live.textContent).toBe("2 selected · 1.0 KB");
  });

  it("says once, on entering a watched folder, that its items can't be renamed", async () => {
    route("GET", "/api/rigs/b/files", (url) =>
      url.includes("share=")
        ? json(200, listing("Autorun", [{ name: "a.fit", rename_refused: r1 }], true))
        : json(200, shareList),
    );
    route("GET", "/api/rigs/c/files", () => json(200, shareList));
    renderPanel("/tools?lrig=b&rrig=c&lshare=EMMC+Images&ldir=Autorun");
    const box = await screen.findByRole("checkbox", { name: "Select a.fit (Bravo)" });
    await flush();
    const left = screen.getByRole("group", { name: "Left pane actions" });
    const live = left.querySelector('[aria-live="polite"]')!;
    expect(live.textContent).toBe("Items here can't be renamed.");
    // The help line leaves rename out where nothing can be renamed.
    expect(screen.getByText("Select files or folders to download, copy or delete.")).toBeTruthy();
    fireEvent.click(box);
    // R1 is the folder's, already said: the selection reads as usual.
    expect(live.textContent).toBe("1 selected · 1.0 KB");
    fireEvent.click(box);
    expect(live.textContent).toBe("");
  });

  it("shows R1 on a visible line where the folder reads unwatched (a turned-off source)", async () => {
    const off = { rule: "watched" as const, message: `${r1.message} (Live belongs to a capture source that is turned off; it still counts while it is configured.)` };
    route("GET", "/api/rigs/b/files", (url) =>
      url.includes("share=") ? json(200, listing("Live", [{ name: "a.fit", rename_refused: off }])) : json(200, shareList),
    );
    route("GET", "/api/rigs/c/files", () => json(200, shareList));
    renderPanel("/tools?lrig=b&rrig=c&lshare=EMMC+Images&ldir=Live");
    expect(screen.queryByText(off.message, { selector: "p" })).toBeNull();
    fireEvent.click(await screen.findByRole("checkbox", { name: "Select a.fit (Bravo)" }));
    expect(screen.getByText(off.message, { selector: "p" })).toBeTruthy();
  });
});

/* ---- rename --------------------------------------------------------------- */

describe("rename dialog", () => {
  beforeEach(() => {
    Element.prototype.scrollIntoView = vi.fn();
  });

  /** Left pane in Preview holding ref.fit and M42; right pane inside Preview/M42. */
  function renderRenamePanel(renamedTo?: { name: string }) {
    let renamed = false;
    route("GET", "/api/rigs/b/files", (url) => {
      if (!url.includes("share=")) return json(200, shareList);
      if (url.includes("dir=Preview%2FM42")) {
        return renamed
          ? json(404, { error: "Not Found", code: "not_found", message: "EMMC Images/Preview/M42 no longer exists on Bravo" })
          : json(200, listing("Preview/M42", [{ name: "a.fit" }]));
      }
      return json(
        200,
        listing("Preview", renamed && renamedTo ? [{ name: renamedTo.name }, { name: "M42", dir: true }] : [{ name: "ref.fit" }, { name: "M42", dir: true }]),
      );
    });
    renderPanel("/tools?lrig=b&rrig=b&lshare=EMMC+Images&ldir=Preview&rshare=EMMC+Images&rdir=Preview%2FM42");
    return { markRenamed: () => (renamed = true) };
  }

  async function openRename(name = "ref.fit") {
    fireEvent.click(await screen.findByRole("checkbox", { name: `Select ${name} (Bravo)` }));
    const left = screen.getByRole("group", { name: "Left pane actions" });
    const button = within(left).getByRole("button", { name: "Rename" });
    button.focus();
    fireEvent.click(button);
    await flush();
    return button;
  }

  it("focuses the field with the stem selected, and returns focus to Rename on close", async () => {
    renderRenamePanel();
    const button = await openRename();
    expect(screen.getByRole("heading", { name: "Rename ref.fit" })).toBeTruthy();
    expect(screen.getByText("in Bravo › EMMC Images › Preview")).toBeTruthy();
    const field = screen.getByRole("textbox", { name: "New name" }) as HTMLInputElement;
    expect(document.activeElement).toBe(field);
    expect(field.value).toBe("ref.fit");
    expect([field.selectionStart, field.selectionEnd]).toEqual([0, 3]);

    fireEvent.keyDown(document, { key: "Escape" });
    await flush();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(button);
  });

  it("validates only on submit, and says an unchanged name is already its name", async () => {
    renderRenamePanel();
    await openRename();
    const field = screen.getByRole("textbox", { name: "New name" });
    fireEvent.change(field, { target: { value: "Dark " } });
    expect(screen.queryByRole("alert")).toBeNull();
    fireEvent.click(within(dialog()).getByRole("button", { name: "Rename" }));
    expect((await screen.findByRole("alert")).textContent).toBe("Names can't end with a space or a dot.");
    fireEvent.change(field, { target: { value: "ref.fit" } });
    expect(screen.queryByRole("alert")).toBeNull();
    fireEvent.submit(field.closest("form")!);
    expect((await screen.findByRole("alert")).textContent).toBe("That is already its name.");
    expect(calls.some((c) => c.url.endsWith("/files/rename"))).toBe(false);
  });

  it("shows exists and watched inline with the field still editable", async () => {
    renderRenamePanel();
    await openRename();
    const answers = [
      json(409, { error: "Conflict", code: "exists", message: "x" }),
      json(409, { error: "Conflict", code: "watched", message: "REF.FIT would be, or would hold, EMMC Images/Autorun." }),
    ];
    route("POST", "/api/rigs/b/files/rename", () => answers.shift()!);
    const field = screen.getByRole("textbox", { name: "New name" });
    fireEvent.change(field, { target: { value: "REF.FIT" } });
    fireEvent.click(within(dialog()).getByRole("button", { name: "Rename" }));
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Bravo already has an item named REF.FIT here (names that differ only in letter case count as the same). Nothing was replaced.",
    );
    expect(calls.find((c) => c.url.endsWith("/files/rename"))!.body).toEqual({
      share: "EMMC Images",
      dir: "Preview",
      name: "ref.fit",
      new_name: "REF.FIT",
    });
    expect(field).toHaveProperty("disabled", false);
    fireEvent.click(within(dialog()).getByRole("button", { name: "Rename" }));
    expect((await screen.findByText("REF.FIT would be, or would hold, EMMC Images/Autorun."))).toBeTruthy();
  });

  it("leaves only Close after not_found, and re-lists the panes", async () => {
    renderRenamePanel();
    await openRename();
    route("POST", "/api/rigs/b/files/rename", () =>
      json(404, { error: "Not Found", code: "not_found", message: "x" }),
    );
    const listsBefore = calls.filter((c) => c.method === "GET").length;
    fireEvent.change(screen.getByRole("textbox", { name: "New name" }), { target: { value: "b.fit" } });
    fireEvent.click(within(dialog()).getByRole("button", { name: "Rename" }));
    expect((await screen.findByRole("alert")).textContent).toMatch(/^ref\.fit isn't in this folder any more\./);
    const footer = dialog().querySelector("footer")!;
    expect(within(footer).getAllByRole("button").map((b) => b.textContent)).toEqual(["Close"]);
    await flush();
    expect(calls.filter((c) => c.method === "GET").length).toBeGreaterThan(listsBefore);
  });

  it("on success selects the renamed entry, scrolls it into view and toasts; the other pane inside a renamed folder reads as gone", async () => {
    const panel = renderRenamePanel({ name: "M42-old" });
    // Rename the folder M42, which the right pane is inside.
    await screen.findByRole("checkbox", { name: "Select a.fit (Bravo)" });
    await openRename("M42");
    const field = screen.getByRole("textbox", { name: "New name" }) as HTMLInputElement;
    // A folder's whole name is selected.
    expect([field.selectionStart, field.selectionEnd]).toEqual([0, 3]);
    route("POST", "/api/rigs/b/files/rename", () => {
      panel.markRenamed();
      return new Response(null, { status: 204 });
    });
    fireEvent.change(field, { target: { value: "M42-old" } });
    fireEvent.click(within(dialog()).getByRole("button", { name: "Rename" }));
    expect(await screen.findByText("Renamed M42 to M42-old")).toBeTruthy();
    const box = (await screen.findByRole("checkbox", { name: "Select M42-old (Bravo)" })) as HTMLInputElement;
    await flush();
    expect(box.checked).toBe(true);
    expect(document.activeElement).toBe(box);
    expect(Element.prototype.scrollIntoView).toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).toBeNull();
    // The right pane does not follow: its folder no longer exists.
    const right = screen.getByRole("group", { name: "Right pane actions" });
    expect(within(right).getByRole("button", { name: "New folder" }).getAttribute("title")).toBe(
      "This folder no longer exists.",
    );
  });
});
