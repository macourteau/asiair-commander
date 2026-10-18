import { describe, expect, it } from "vitest";
import type { RigStatus, SafeToPowerOff, Snapshot } from "@/api";
import { safeTransitions } from "./safeAnnounce";

function snap(...rigs: [string, SafeToPowerOff][]): Snapshot {
  return {
    rigs: rigs.map(([name, safe]) => ({ id: `id-${name}`, name, safe_to_power_off: safe }) as RigStatus),
    nas_free: 0,
    bytes_last_12h: 0,
    unacked_alerts: 0,
    generated_at: 0,
  };
}

describe("safeTransitions", () => {
  it("announces nothing for the first snapshot", () => {
    expect(safeTransitions(null, snap(["Rig A", "safe"]))).toEqual([]);
  });

  it("announces a rig becoming safe and one leaving safe", () => {
    expect(
      safeTransitions(
        snap(["Rig A", "working"], ["Rig B", "safe"]),
        snap(["Rig A", "safe"], ["Rig B", "working"]),
      ),
    ).toEqual(["Rig A is safe to power off.", "Rig B is no longer safe to power off."]);
  });

  it("stays quiet when the verdict does not cross safe", () => {
    expect(safeTransitions(snap(["Rig A", "working"]), snap(["Rig A", "unknown"]))).toEqual([]);
    expect(safeTransitions(snap(["Rig A", "safe"]), snap(["Rig A", "safe"]))).toEqual([]);
  });

  it("ignores rigs added or removed between snapshots", () => {
    expect(safeTransitions(snap(["Rig A", "safe"]), snap(["Rig B", "safe"]))).toEqual([]);
  });
});
