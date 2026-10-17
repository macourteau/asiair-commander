import { describe, expect, it } from "vitest";
import { showSafeAsOf } from "./safeAsOf";

describe("showSafeAsOf", () => {
  it("dates a paused rig's Yes or Working verdict", () => {
    expect(showSafeAsOf(true, "safe", undefined, 1700000000)).toBe(true);
    expect(showSafeAsOf(true, "working", undefined, 1700000000)).toBe(true);
  });

  it("dates a paused rig's Unknown only when a listing itself produced it", () => {
    expect(showSafeAsOf(true, "unknown", "missing_on_device", 1700000000)).toBe(true);
    expect(showSafeAsOf(true, "unknown", "removable_absent", 1700000000)).toBe(true);
    expect(showSafeAsOf(true, "unknown", "listing_failed", 1700000000)).toBe(true);
  });

  it("never dates the Unknown a missing listing produced", () => {
    expect(showSafeAsOf(true, "unknown", "paused", 1700000000)).toBe(false);
    expect(showSafeAsOf(true, "unknown", "offline", 1700000000)).toBe(false);
    expect(showSafeAsOf(true, "unknown", "offline_during_run", 1700000000)).toBe(false);
    expect(showSafeAsOf(true, "unknown", "identity", 1700000000)).toBe(false);
  });

  it("never shows without a safe_as_of, or on an unpaused rig", () => {
    expect(showSafeAsOf(true, "safe", undefined, undefined)).toBe(false);
    expect(showSafeAsOf(true, "safe", undefined, 0)).toBe(false);
    expect(showSafeAsOf(false, "safe", undefined, 1700000000)).toBe(false);
  });
});
