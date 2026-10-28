// @vitest-environment jsdom
/**
 * The missing panel against a stubbed fetch answering the server's envelope
 * (design library-reconciliation §13.3).
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ToastProvider } from "@/design";
import type { Frame, MissingFramesPage, MissingSection } from "@/api";
import { MissingFrames } from "./MissingFrames";

const urls: string[] = [];

function envelope(section: MissingSection, over: Partial<MissingFramesPage> = {}): MissingFramesPage {
  return {
    frames: [],
    page: 1,
    page_size: 50,
    total: 0,
    section,
    groups: [],
    totals: { actionable: 0, leftover: 3 },
    ...over,
  };
}

let pages: Record<MissingSection, MissingFramesPage>;
/** Each POST the panel sent, its body parsed. */
let posts: { url: string; body: Record<string, unknown> | undefined }[];
/** Answers a POST; the default refuses it, so a test states every write it expects. */
let onPost: (url: string, body: Record<string, unknown> | undefined) => Response;

const json = (status: number, body?: unknown) =>
  new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

beforeEach(() => {
  urls.length = 0;
  posts = [];
  onPost = (url) => {
    throw new Error(`unexpected POST ${url}`);
  };
  pages = { actionable: envelope("actionable"), leftover: envelope("leftover") };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      urls.push(url);
      if (init?.method === "POST") {
        const body = init.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined;
        posts.push({ url, body });
        return onPost(url, body);
      }
      const section = (new URL(url, "http://x").searchParams.get("section") ?? "actionable") as MissingSection;
      return json(200, pages[section]);
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

/** A leftover row as the server sends it: deleted, marked missing. */
function leftoverRow(id: number, over: Partial<Frame> = {}): Frame {
  return {
    id,
    rig_id: "rig-a",
    source_share: "Udisk",
    source_path: `/Autorun/Light/IC 434/Light_${id}.fit`,
    size: 1,
    mtime: 1,
    frame_type: "Light",
    exptime: 60,
    set_temp: -10,
    gain: 100,
    offset: 10,
    night: 1,
    dest_path: `/lib/IC 434/NIGHT_1/Light_${id}.fit`,
    state: "deleted",
    attempts: 0,
    missing_seen_at: 1_700_000_000,
    ...over,
  };
}

/** Lets fetches and the state updates they cause settle. */
async function flush() {
  await act(async () => {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  });
}

/** Opens the collapsed leftovers disclosure and waits for its first page. */
async function openLeftovers() {
  const details = (await screen.findByText(/^Already gone from the device/)).closest("details")!;
  details.open = true;
  fireEvent(details, new Event("toggle"));
  await waitFor(() => expect(urls.some((u) => u.includes("section=leftover"))).toBe(true));
  await flush();
  return details;
}

const renderPanel = (missingWarning?: string, keepOriginals = false) =>
  render(
    <ToastProvider>
      <MissingFrames rigId="rig-a" keepOriginals={keepOriginals} missingWarning={missingWarning} />
    </ToastProvider>,
  );

describe("MissingFrames", () => {
  it("points an empty actionable section at the leftovers while the warning is up", async () => {
    renderPanel("Frames are missing from the NAS.");
    expect(await screen.findByText(/the frame it refers to is under Already gone from the device/)).toBeTruthy();
    expect(screen.getByText("Already gone from the device (3)")).toBeTruthy();
    // The leftovers are collapsed and not fetched until opened.
    expect(urls.every((u) => !u.includes("section=leftover"))).toBe(true);
  });

  it("says plainly that nothing needs action without the warning", async () => {
    renderPanel();
    expect(await screen.findByText("No missing frames need action.")).toBeTruthy();
  });

  it("offers Acknowledge all only where something is acknowledgeable, naming that count", async () => {
    pages.actionable = envelope("actionable", {
      groups: [
        { dir: "/lib/NGC 7000/NIGHT_3", count: 3, ackable: 2 },
        { dir: "/lib/M31/NIGHT_2", count: 1, ackable: 0 },
      ],
      totals: { actionable: 4, leftover: 0 },
    });
    renderPanel();
    expect(await screen.findByRole("button", { name: "Acknowledge all 2" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Acknowledge all 3/ })).toBeNull();
    expect(screen.getAllByRole("button", { name: /^Acknowledge all/ })).toHaveLength(1);
    expect(screen.getByText("Still on the device (4)")).toBeTruthy();
  });

  it("renders no Acknowledge all on a keep_originals rig", async () => {
    pages.actionable = envelope("actionable", {
      groups: [{ dir: "/lib/IC 1805/NIGHT_5", count: 1, ackable: 1 }],
      totals: { actionable: 1, leftover: 0 },
    });
    renderPanel(undefined, true);
    expect(await screen.findByRole("button", { name: /IC 1805/ })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Acknowledge all/ })).toBeNull();
  });

  it("names the leftover total on the section's Forget all, and no count on the age scopes", async () => {
    pages.leftover = envelope("leftover", { total: 60, frames: [leftoverRow(1)], totals: { actionable: 0, leftover: 60 } });
    pages.actionable = envelope("actionable", { totals: { actionable: 0, leftover: 60 } });
    renderPanel();
    const details = await openLeftovers();
    const d = within(details);
    expect(d.getByRole("button", { name: "Forget all 60" })).toBeTruthy();
    expect(d.getByRole("button", { name: "Forget older than 7 days" })).toBeTruthy();
    expect(d.getByRole("button", { name: "Forget older than 28 days" })).toBeTruthy();
  });

  it("never shows the empty state over rows that still exist", async () => {
    // An answer with no rows on the page but rows in the section (a page the
    // server has not yet stepped back from) must not say there is nothing left.
    pages.leftover = envelope("leftover", { page: 2, total: 50, totals: { actionable: 0, leftover: 50 } });
    renderPanel();
    await openLeftovers();
    expect(screen.queryByText("Nothing left to forget.")).toBeNull();
    expect(screen.getByText(/50 leftovers · page 2 of 1/)).toBeTruthy();
  });

  it("clears the folder filter when its folder leaves the section", async () => {
    const dirA = "/lib/IC 434/NIGHT_1";
    const dirB = "/lib/M42/NIGHT_2";
    pages.leftover = envelope("leftover", {
      total: 2,
      frames: [leftoverRow(1)],
      groups: [
        { dir: dirA, count: 1, ackable: 0 },
        { dir: dirB, count: 1, ackable: 0 },
      ],
      totals: { actionable: 0, leftover: 2 },
    });
    renderPanel();
    const details = await openLeftovers();
    fireEvent.click(within(details).getByRole("button", { name: new RegExp(dirA) }));
    await waitFor(() =>
      expect(within(details).getByRole("button", { name: `Show every folder, not only ${dirA}` })).toBeTruthy(),
    );

    // The folder is forgotten elsewhere; the next answer no longer lists it.
    pages.leftover = envelope("leftover", {
      total: 1,
      groups: [{ dir: dirB, count: 1, ackable: 0 }],
      totals: { actionable: 0, leftover: 1 },
    });
    fireEvent.change(within(details).getByRole("searchbox"), { target: { value: "light" } });
    await waitFor(() => expect(within(details).queryByRole("button", { name: /Show every folder/ })).toBeNull());
    const last = urls[urls.length - 1]!;
    expect(new URL(last, "http://x").searchParams.get("dir") ?? "").toBe("");
  });

  describe("Forget", () => {
    const forgetURL = "/api/rigs/rig-a/missing-leftovers/forget";

    beforeEach(() => {
      pages.actionable = envelope("actionable", { totals: { actionable: 0, leftover: 3 } });
      pages.leftover = envelope("leftover", {
        total: 3,
        frames: [leftoverRow(1), leftoverRow(2, { restore_requested_at: 1_700_000_100 })],
        groups: [
          { dir: "/lib/IC 434/NIGHT_1", count: 2, ackable: 0 },
          { dir: "/lib/M42/NIGHT_2", count: 1, ackable: 0 },
        ],
        totals: { actionable: 0, leftover: 3 },
      });
    });

    it("shows the server's count and skips, and confirms with the minted scope and token", async () => {
      onPost = (url, body) => {
        expect(url).toBe(forgetURL);
        return body?.token
          ? json(200, { forgotten: 1 })
          : json(200, { token: "tok-1", expires_at: 1, count: 1, skipped: { restore_pending: 1 } });
      };
      renderPanel();
      const details = await openLeftovers();
      fireEvent.click(within(details).getByRole("button", { name: "Forget all 2" }));
      const dlg = await screen.findByRole("dialog");
      expect(within(dlg).getByText("Forget 1 frame?")).toBeTruthy();
      expect(within(dlg).getByText(/Not included: 1 with a pending\s+restore/)).toBeTruthy();
      expect(posts[0]!.body).toEqual({ dir: "/lib/IC 434/NIGHT_1" });

      const before = urls.length;
      fireEvent.click(within(dlg).getByRole("button", { name: "Forget" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect(posts[1]!.body).toEqual({ dir: "/lib/IC 434/NIGHT_1", token: "tok-1" });
      expect(await screen.findByText("Forgot 1 frame.")).toBeTruthy();
      // Both sections refresh.
      await waitFor(() => expect(urls.slice(before).filter((u) => !u.includes("/forget")).length).toBe(2));
    });

    it("carries an age scope through the challenge and the confirm", async () => {
      onPost = (_url, body) =>
        body?.token
          ? json(200, { forgotten: 1 })
          : json(200, { token: "tok-7", expires_at: 1, count: 1, skipped: { restore_pending: 0 } });
      renderPanel();
      const details = await openLeftovers();
      fireEvent.click(within(details).getByRole("button", { name: "Forget older than 7 days" }));
      const dlg = await screen.findByRole("dialog");
      expect(within(dlg).queryByText(/Not included/)).toBeNull();
      fireEvent.click(within(dlg).getByRole("button", { name: "Forget" }));
      await waitFor(() => expect(posts).toHaveLength(2));
      expect(posts[0]!.body).toEqual({ older_than_days: 7 });
      expect(posts[1]!.body).toEqual({ older_than_days: 7, token: "tok-7" });
    });

    it("treats 202 FORGET_QUEUED as success and refreshes", async () => {
      onPost = (_url, body) =>
        body?.token
          ? json(202, { code: "FORGET_QUEUED", message: "queued" })
          : json(200, { token: "tok-1", expires_at: 1, count: 2, skipped: { restore_pending: 0 } });
      renderPanel();
      const details = await openLeftovers();
      fireEvent.click(within(details).getByRole("button", { name: "Forget all 3" }));
      const dlg = await screen.findByRole("dialog");
      const before = urls.length;
      fireEvent.click(within(dlg).getByRole("button", { name: "Forget" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect(await screen.findByText("The rig is busy; this may already have applied.")).toBeTruthy();
      expect(screen.queryByRole("alert")).toBeNull();
      await waitFor(() => expect(urls.slice(before).filter((u) => !u.includes("/forget")).length).toBe(2));
    });

    it("closes the dialog on a refused token and says so", async () => {
      onPost = (_url, body) =>
        body?.token
          ? json(403, { code: "FORGET_CONFIRMATION_INVALID", message: "Confirmation expired." })
          : json(200, { token: "tok-1", expires_at: 1, count: 2, skipped: { restore_pending: 0 } });
      renderPanel();
      const details = await openLeftovers();
      fireEvent.click(within(details).getByRole("button", { name: "Forget all 3" }));
      const dlg = await screen.findByRole("dialog");
      fireEvent.click(within(dlg).getByRole("button", { name: "Forget" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      expect((await screen.findByRole("alert")).textContent).toMatch(/That confirmation expired/);
    });

    it("disables the row Forget on a restore-pending row", async () => {
      renderPanel();
      const details = await openLeftovers();
      const rowButtons = within(details).getAllByRole("button", { name: "Forget" }) as HTMLButtonElement[];
      expect(rowButtons.map((b) => b.disabled)).toEqual([false, true]);
      expect(rowButtons[1]!.title).toBe("Cancel the pending restore first");
    });
  });
});
