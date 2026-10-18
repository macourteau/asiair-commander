import { describe, expect, it } from "vitest";

import { stuckFrameHelp, whyItStaysLabel } from "./stuckFrame";

const now = Date.parse("2026-10-07T23:15:02Z");

describe("stuckFrameHelp", () => {
  it("offers no dismiss for a held frame, and says why in plain words", () => {
    const h = stuckFrameHelp({ state: "held" }, now);
    expect(h?.canDismiss).toBe(false);
    expect(h?.why).not.toMatch(/three-way|delete-pass|mismatch/);
    expect(h?.next).toMatch(/nothing to dismiss/);
    // A held frame whose device file is unchanged is never re-checked (design
    // frame-lifecycle §4), so the help must not promise that it clears itself.
    expect(h?.next).not.toMatch(/on its own once/);
    expect(h?.next).toMatch(/File browser/);
  });

  it("starts the button's accessible name with its visible text", () => {
    expect(whyItStaysLabel("Light_0001.fit", "FRA300 Rig")).toBe("Why it stays: Light_0001.fit on FRA300 Rig");
  });

  it("names the next retry of a failed frame only when it is still ahead", () => {
    const ahead = stuckFrameHelp({ state: "failed", next_retry_at: now / 1000 + 600 }, now);
    expect(ahead?.next).toContain("The next try is in 10 mins.");
    expect(ahead?.canDismiss).toBe(false);
    const past = stuckFrameHelp({ state: "failed", next_retry_at: now / 1000 - 600 }, now);
    expect(past?.next).not.toContain("next try");
  });

  it("tells the operator Run now retries a failed frame, and offers it once no retry is ahead", () => {
    const ahead = stuckFrameHelp({ state: "failed", next_retry_at: now / 1000 + 600 }, now);
    expect(ahead?.next).toMatch(/Run now on this rig gives it one more try/);
    expect(ahead?.canRunNow).toBe(false);
    // An exhausted frame records no next retry (design frame-lifecycle §4.3).
    const exhausted = stuckFrameHelp({ state: "failed" }, now);
    expect(exhausted?.next).toMatch(/Run now on this rig gives it one more try/);
    expect(exhausted?.canRunNow).toBe(true);
    expect(stuckFrameHelp({ state: "held" }, now)?.canRunNow).toBe(false);
    expect(stuckFrameHelp({ state: "unclassified" }, now)?.canRunNow).toBe(false);
  });

  it("offers dismiss for an unclassified frame unless it is culled", () => {
    expect(stuckFrameHelp({ state: "unclassified" }, now)?.canDismiss).toBe(true);
    expect(
      stuckFrameHelp({ state: "unclassified", missing_seen_at: now / 1000 }, now)?.canDismiss,
    ).toBe(false);
  });

  it("has nothing to say about a frame that is not stuck", () => {
    expect(stuckFrameHelp({ state: "verified" }, now)).toBeNull();
    expect(stuckFrameHelp({ state: "deleted" }, now)).toBeNull();
  });
});
