import { describe, expect, it } from "vitest";
import { cleanupPct, cleanupPosition, isWorking } from "./rigActivity";

describe("isWorking", () => {
  it("is true only while a copy or cleanup is live", () => {
    expect(isWorking({ current: { file: "a.fit", pct: 30 } })).toBe(true);
    expect(isWorking({ cleanup: { file: "a.fit", index: 1, total: 2 } })).toBe(true);
    // Between cycles the state still reads the last phase, but nothing is live.
    expect(isWorking({ current: null, cleanup: null })).toBe(false);
    expect(isWorking({})).toBe(false);
  });
});

describe("cleanup position", () => {
  it("reads as the frame being checked out of the pass", () => {
    expect(cleanupPosition({ file: "a.fit", index: 12, total: 50 })).toBe("12 of 50");
  });

  it("counts only the frames already behind the current one", () => {
    expect(cleanupPct({ file: "a.fit", index: 1, total: 50 })).toBe(0);
    expect(cleanupPct({ file: "a.fit", index: 26, total: 50 })).toBe(50);
  });

  it("never overflows the bar on a malformed position", () => {
    expect(cleanupPct({ file: "a.fit", index: 9, total: 0 })).toBe(0);
    expect(cleanupPct({ file: "a.fit", index: 99, total: 10 })).toBe(100);
  });
});
