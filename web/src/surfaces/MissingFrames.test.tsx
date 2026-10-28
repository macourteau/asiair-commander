// @vitest-environment jsdom
/**
 * The missing panel against a stubbed fetch answering the server's envelope
 * (design library-reconciliation §13.3).
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { ToastProvider } from "@/design";
import type { MissingFramesPage, MissingSection } from "@/api";
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

beforeEach(() => {
  urls.length = 0;
  pages = { actionable: envelope("actionable"), leftover: envelope("leftover") };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      urls.push(url);
      const section = (new URL(url, "http://x").searchParams.get("section") ?? "actionable") as MissingSection;
      return new Response(JSON.stringify(pages[section]), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

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
});
