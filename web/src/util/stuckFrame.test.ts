import { describe, expect, it } from "vitest";

import { stuckFrameHelp } from "./stuckFrame";

const now = Date.parse("2026-10-07T23:15:02Z");

describe("stuckFrameHelp", () => {
  it("offers no dismiss for a held frame, and says why in plain words", () => {
    const h = stuckFrameHelp({ state: "held" }, now);
    expect(h?.canDismiss).toBe(false);
    expect(h?.why).not.toMatch(/three-way|delete-pass|mismatch/);
    expect(h?.next).toMatch(/nothing to dismiss/);
  });

  it("names the next retry of a failed frame only when it is still ahead", () => {
    const ahead = stuckFrameHelp({ state: "failed", next_retry_at: now / 1000 + 600 }, now);
    expect(ahead?.next).toContain("The next try is in 10 mins.");
    expect(ahead?.canDismiss).toBe(false);
    const past = stuckFrameHelp({ state: "failed", next_retry_at: now / 1000 - 600 }, now);
    expect(past?.next).not.toContain("next try");
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
