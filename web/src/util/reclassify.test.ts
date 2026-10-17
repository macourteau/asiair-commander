import { describe, expect, it } from "vitest";
import { reclassifyOutcome } from "./reclassify";

const outcome = (examined: number, moved: number) =>
  reclassifyOutcome({ rig_id: "rig-fra", examined, moved });

describe("reclassifyOutcome", () => {
  it("reports how many of the held frames were re-filed", () => {
    expect(outcome(50, 50)).toBe(
      "Reclassify finished — re-filed all 50 held frames into the library.",
    );
    expect(outcome(50, 12)).toBe(
      "Reclassify finished — re-filed 12 of 50 held frames; the rest still need an alias.",
    );
  });

  it("names the fix when the pass moved nothing", () => {
    // The case that reads as a broken button: the operator triggers a reclassify,
    // sees no transfer (there is none to see) and no change in the held table.
    expect(outcome(50, 0)).toContain("none matched a known type");
    expect(outcome(50, 0)).toContain("alias tables in Settings");
  });

  it("says so plainly when nothing was held", () => {
    expect(outcome(0, 0)).toBe("Reclassify finished — no frames were being held.");
  });

  it("keeps the singular readable", () => {
    expect(outcome(1, 0)).toContain("rechecked 1 held frame,");
    expect(outcome(1, 1)).toBe(
      "Reclassify finished — re-filed the held frame into the library.",
    );
  });
});
