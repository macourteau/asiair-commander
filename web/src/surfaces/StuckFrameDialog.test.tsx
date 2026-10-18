// @vitest-environment jsdom
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { ToastProvider } from "@/design";
import { api, type Frame } from "@/api";
import { StuckFrameDialog } from "./StuckFrameDialog";

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
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function frame(over: Partial<Frame>): Frame {
  return {
    id: 7,
    rig_id: "rig-fra",
    source_share: "EMMC Images",
    source_path: "Autorun/Light/M42/Light_0001.fit",
    state: "held",
    attempts: 0,
    size: 1,
    mtime: 1,
    ...over,
  } as Frame;
}

function show(f: Frame, onDismissed = () => {}, onClose = () => {}) {
  render(
    <ToastProvider>
      <MemoryRouter>
        <StuckFrameDialog frame={f} rigLabel="FRA300 Rig" onClose={onClose} onDismissed={onDismissed} />
      </MemoryRouter>
    </ToastProvider>,
  );
}

describe("StuckFrameDialog", () => {
  it("explains a held frame plainly, keeps the recorded error behind a disclosure, and offers no dismiss", () => {
    const error = "delete-pass three-way check failed (device/NAS/recorded mismatch)";
    show(frame({ state: "held", error }));
    expect(screen.getByRole("dialog", { name: "Why Light_0001.fit stays on FRA300 Rig" })).toBeTruthy();
    expect(screen.getByText(/nothing to dismiss/)).toBeTruthy();
    expect(screen.getByText(error).closest("details")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Dismiss frame" })).toBeNull();
  });

  it("offers no dismiss for a failed frame", () => {
    show(frame({ state: "failed", error: "stream copy failed; will retry" }));
    expect(screen.queryByRole("button", { name: "Dismiss frame" })).toBeNull();
  });

  it("retries a failed frame that used up its attempts through the rig's Run now", async () => {
    const runNow = vi.spyOn(api, "runRigNow").mockResolvedValue(undefined);
    const onClose = vi.fn();
    show(frame({ state: "failed", attempts: 6 }), () => {}, onClose);
    expect(screen.getByText(/Run now on this rig gives it one more try/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Run now" }));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(runNow).toHaveBeenCalledWith("rig-fra");
  });

  it("offers no Run now while a failed frame's own retry is still ahead", () => {
    show(frame({ state: "failed", attempts: 1, next_retry_at: Date.now() / 1000 + 600 }));
    expect(screen.getByText(/The next try is/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Run now" })).toBeNull();
  });

  it("dismisses an unclassified frame through the existing API and says the device file stays", async () => {
    const discard = vi.spyOn(api, "discardFrame").mockResolvedValue(undefined);
    const onDismissed = vi.fn();
    const onClose = vi.fn();
    show(frame({ state: "unclassified" }), onDismissed, onClose);
    expect(screen.getByText(/does/).textContent).toMatch(/not delete the file from the device/);
    expect(
      screen.getByRole("link", { name: "Reclassify FRA300 Rig's frames in Devices" }).getAttribute("href"),
    ).toBe("/devices?rig=rig-fra&show=unclassified");
    fireEvent.click(screen.getByRole("button", { name: "Dismiss frame" }));
    await waitFor(() => expect(onDismissed).toHaveBeenCalled());
    expect(discard).toHaveBeenCalledWith("rig-fra", 7);
    expect(onClose).toHaveBeenCalled();
  });
});
