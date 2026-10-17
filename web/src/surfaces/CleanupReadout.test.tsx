// @vitest-environment jsdom
/**
 * The card must never read as stalled while the rig works: a delete pass shows
 * where it is, and the idle "Waiting for frames…" hint stays away while a copy or
 * cleanup is live (it still shows on a rig with no capture cadence otherwise).
 */
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { CleanupReadout } from "./CleanupReadout";
import { NextFrame } from "./NextFrame";

afterEach(cleanup);

describe("CleanupReadout", () => {
  it("shows the frame being checked and the pass position", () => {
    render(<CleanupReadout cleanup={{ file: "Bias_0012.fit", index: 12, total: 50 }} />);
    expect(screen.getByText("12 of 50")).toBeTruthy();
    expect(screen.getByText("Bias_0012.fit")).toBeTruthy();
    expect(screen.getByRole("progressbar").getAttribute("aria-valuenow")).toBe("22");
  });

  it("renders nothing outside the pass", () => {
    const { container } = render(<CleanupReadout cleanup={null} />);
    expect(container.innerHTML).toBe("");
  });
});

describe("NextFrame without a cadence", () => {
  it("hints at waiting only while the rig is idle", () => {
    render(<NextFrame prediction={null} online paused={false} working={false} />);
    expect(screen.getByText("Waiting for frames…")).toBeTruthy();
  });

  it("says nothing while a copy or cleanup is live", () => {
    const { container } = render(<NextFrame prediction={null} online paused={false} working />);
    expect(container.innerHTML).toBe("");
  });
});
