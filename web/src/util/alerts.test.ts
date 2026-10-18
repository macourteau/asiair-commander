import { describe, expect, it } from "vitest";

import { ackButtonLabel, alertFramesLink, nextAckFocus, rigDisplayName } from "./alerts";

describe("rigDisplayName", () => {
  const rigs = [{ id: "fra300", name: "FRA300 Rig" }];
  it("names a configured rig by its display name", () => {
    expect(rigDisplayName("fra300", rigs)).toBe("FRA300 Rig");
  });
  it("falls back to the id for a rig no longer configured", () => {
    expect(rigDisplayName("gone", rigs)).toBe("gone");
  });
});

describe("alertFramesLink", () => {
  it("links a rig's backlog alert to its never-deletable frames in History", () => {
    expect(alertFramesLink({ category: "backlog", rig_id: "fra 300" })).toBe(
      "/history?rig=fra+300&view=never_deletable",
    );
  });
  it("gives other alerts, and a backlog with no rig, no link", () => {
    expect(alertFramesLink({ category: "nas_disk_low", rig_id: "" })).toBeNull();
    expect(alertFramesLink({ category: "device_disk_low", rig_id: "r" })).toBeNull();
    expect(alertFramesLink({ category: "backlog", rig_id: "" })).toBeNull();
  });
});

describe("ackButtonLabel", () => {
  it("names the alert's subject", () => {
    expect(ackButtonLabel("Backlog building up", "FRA300 Rig")).toBe(
      "Acknowledge: Backlog building up on FRA300 Rig",
    );
    expect(ackButtonLabel("NAS disk low", null)).toBe("Acknowledge: NAS disk low");
  });
});

describe("nextAckFocus", () => {
  it("moves to the next alert down the list", () => {
    expect(nextAckFocus([5, 4, 3], 4)).toBe(3);
  });
  it("moves up from the last alert", () => {
    expect(nextAckFocus([5, 4, 3], 3)).toBe(4);
  });
  it("returns null when the acknowledged alert was the last one", () => {
    expect(nextAckFocus([7], 7)).toBeNull();
  });
  it("picks the first remaining alert when the acknowledged one is not listed", () => {
    expect(nextAckFocus([5, 4], 9)).toBe(5);
  });
});
