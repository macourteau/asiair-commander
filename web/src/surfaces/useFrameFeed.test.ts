// @vitest-environment jsdom
/**
 * The feed's row order. The "When" column renders a frame's transfer time, so the
 * list has to be ordered by that same time: a row placed by anything else (the
 * order the client happened to discover it, the order an event arrived) shows an
 * older timestamp above a newer one the moment the two disagree — which is what an
 * operator reads as "the feed is out of sequence".
 *
 * The disagreements are real: a completion event can arrive long after the transfer
 * it reports (a late plate-solve, a re-file), a slow transfer finishes after frames
 * discovered later, and a reconnect rebuilds the server-side rows underneath rows
 * that arrived live hours earlier.
 */
import { describe, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { Frame, Snapshot } from "@/api";

const listTransferHistory = vi.fn();
const listFrames = vi.fn();
const handlers = new Map<string, (data: unknown) => void>();
let snapshot: Snapshot | null = null;

vi.mock("@/api", () => ({
  api: {
    listTransferHistory: (...args: unknown[]) => listTransferHistory(...args),
    listFrames: (...args: unknown[]) => listFrames(...args),
  },
  useSnapshot: () => snapshot,
  useEventSubscription: (name: string, handler: (data: unknown) => void) => {
    handlers.set(name, handler);
  },
}));

import { useFrameFeed } from "./useFrameFeed";
import { frameTime } from "../util/frameTime";

const NOW = 1_800_000_000;
const MIN = 60;
const HOUR = 60 * MIN;

const frame = (path: string, pulledAt: number, over: Partial<Frame> = {}): Frame =>
  ({
    id: 1,
    rig_id: "rig1",
    source_share: "EMMC Images",
    source_path: path,
    size: 116_700_000,
    mtime: pulledAt - MIN,
    frame_type: "LIGHT",
    exptime: 600,
    set_temp: 0,
    gain: 100,
    offset: 0,
    night: 1,
    state: "verified",
    attempts: 1,
    pulled_at: pulledAt,
    ...over,
  }) as Frame;

async function mount(seed: Frame[]) {
  snapshot = null;
  handlers.clear();
  listTransferHistory.mockClear();
  listTransferHistory.mockResolvedValue({
    frames: seed,
    total: seed.length,
    page: 1,
    page_size: 200,
  });
  const view = renderHook(() => useFrameFeed());
  await waitFor(() => expect(view.result.current.loading).toBe(false));
  return view;
}

const emit = async (name: string, data: unknown) => {
  await act(async () => {
    handlers.get(name)?.(data);
  });
};

const paths = (rows: { path: string }[]) => rows.map((r) => r.path);
const rowFor = <T extends { path: string }>(rows: T[], path: string) =>
  rows.find((r) => r.path === path)!;

/** The invariant the surface promises: the When column never rises going down. */
const expectDescending = (rows: { frame?: Frame }[]) => {
  const times = rows.map((r) => frameTime(r.frame)).filter((t) => t > 0);
  expect(times).toEqual([...times].sort((a, b) => b - a));
};

describe("useFrameFeed ordering", () => {
  it("files a late completion for an old frame by its transfer time", async () => {
    const view = await mount([
      frame("/recent-11m.fit", NOW - 11 * MIN),
      frame("/recent-20m.fit", NOW - 20 * MIN),
    ]);

    // A frame pulled four hours ago completes its solve (or is re-filed) now: the
    // event carries the ORIGINAL pulled_at, not the moment it arrived.
    await emit("frame.done", frame("/old-4h.fit", NOW - 4 * HOUR, { state: "deleted" }));

    expect(paths(view.result.current.rows)).toEqual([
      "/recent-11m.fit",
      "/recent-20m.fit",
      "/old-4h.fit",
    ]);
    expectDescending(view.result.current.rows);
  });

  it("orders two completions by transfer time, not by arrival", async () => {
    const view = await mount([]);

    // A slow transfer discovered first finishes last: its completion arrives after
    // a frame that was pulled earlier.
    await emit("frame.done", frame("/slow.fit", NOW - 1 * MIN));
    await emit("frame.done", frame("/quick.fit", NOW - 10 * MIN));

    expect(paths(view.result.current.rows)).toEqual(["/slow.fit", "/quick.fit"]);
    expectDescending(view.result.current.rows);
  });

  it("keeps live rows, which have no transfer time yet, above completed ones", async () => {
    const view = await mount([frame("/filed.fit", NOW - 2 * MIN)]);

    snapshot = {
      rigs: [
        {
          id: "rig1",
          pending_listed: true,
          pending: [
            { source_share: "EMMC Images", source_path: "/pending-a.fit", size: 1 },
            { source_share: "EMMC Images", source_path: "/pending-b.fit", size: 1 },
          ],
        },
      ],
    } as unknown as Snapshot;
    await act(async () => view.rerender());

    // Newest discovery on top within the live group; the filed frame sits below.
    expect(paths(view.result.current.rows)).toEqual([
      "/pending-b.fit",
      "/pending-a.fit",
      "/filed.fit",
    ]);
  });
});

describe("useFrameFeed reseeding", () => {
  it("adopts the server's state for a live row whose completion it missed", async () => {
    const view = await mount([frame("/other.fit", NOW - 5 * MIN)]);

    // Watched live: filed, then held for its deletion grace window.
    await emit("frame.done", frame("/watched.fit", NOW - 4 * HOUR));
    expect(rowFor(view.result.current.rows, "/watched.fit").display).toEqual({
      kind: "final",
      state: "verified",
    });

    // The original was deleted while the stream was down, so the frame.deleted
    // event never arrived. The reconnect seed is the only place that truth exists.
    listTransferHistory.mockResolvedValue({
      frames: [
        frame("/other.fit", NOW - 5 * MIN),
        frame("/watched.fit", NOW - 4 * HOUR, { state: "deleted" }),
      ],
      total: 2,
      page: 1,
      page_size: 200,
    });
    await emit("reconnect", undefined);

    await waitFor(() =>
      expect(rowFor(view.result.current.rows, "/watched.fit").display).toEqual({
        kind: "final",
        state: "deleted",
      }),
    );
  });

  it("leaves a row that is still in flight on its live state", async () => {
    // A re-pulled frame: the server still holds the PREVIOUS pull's completed row
    // while the file is being copied again, so the seed must not turn the live row
    // final under the operator (design's re-adoption case, decide.go:51-55).
    const view = await mount([]);

    snapshot = {
      rigs: [
        {
          id: "rig1",
          pending_listed: true,
          pending: [{ source_share: "EMMC Images", source_path: "/repull.fit", size: 1 }],
        },
      ],
    } as unknown as Snapshot;
    await act(async () => view.rerender());
    expect(rowFor(view.result.current.rows, "/repull.fit").display).toEqual({ kind: "pending" });

    listTransferHistory.mockResolvedValue({
      frames: [frame("/repull.fit", NOW - 3 * HOUR)],
      total: 1,
      page: 1,
      page_size: 200,
    });
    await emit("reconnect", undefined);
    await waitFor(() => expect(listTransferHistory).toHaveBeenCalledTimes(2));

    expect(rowFor(view.result.current.rows, "/repull.fit").display).toEqual({ kind: "pending" });
  });
});
