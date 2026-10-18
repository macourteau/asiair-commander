import { describe, expect, it } from "vitest";

import {
  ackButtonLabel,
  alertFramesLink,
  alertsForRig,
  nextAckFocus,
  rigAlertsLink,
  rigDisplayName,
} from "./alerts";

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
    expect(alertFramesLink({ category: "backlog", rig_id: "fra 300" }, "FRA300 Rig")).toEqual({
      to: "/history?rig=fra+300&view=never_deletable",
      text: "Show these frames in History",
      label: "Show FRA300 Rig's frames that can't be removed, in History",
    });
  });
  it("links a rig's library_reconciled alert to its missing frames in Devices", () => {
    expect(alertFramesLink({ category: "library_reconciled", rig_id: "rig-fra" }, "FRA300 Rig")).toEqual({
      to: "/devices?rig=rig-fra&show=missing",
      text: "Show missing frames in Devices",
      label: "Show FRA300 Rig's missing frames, in Devices",
    });
  });
  it("names each rig in its link, so two alerts' links differ", () => {
    const a = alertFramesLink({ category: "backlog", rig_id: "a" }, "FRA300 Rig");
    const b = alertFramesLink({ category: "backlog", rig_id: "b" }, "Newtonian Rig");
    expect(a?.label).not.toBe(b?.label);
  });
  it("gives other alerts, and an alert with no rig, no link", () => {
    expect(alertFramesLink({ category: "nas_disk_low", rig_id: "" }, "")).toBeNull();
    expect(alertFramesLink({ category: "device_disk_low", rig_id: "r" }, "R")).toBeNull();
    expect(alertFramesLink({ category: "backlog", rig_id: "" }, "")).toBeNull();
  });
});

describe("alertsForRig", () => {
  const alerts = [{ rig_id: "a" }, { rig_id: "b" }, { rig_id: "" }];
  it("keeps only the rig's alerts", () => {
    expect(alertsForRig(alerts, "a")).toEqual([{ rig_id: "a" }]);
  });
  it("keeps every alert with no rig chosen", () => {
    expect(alertsForRig(alerts, "")).toHaveLength(3);
  });
});

describe("rigAlertsLink", () => {
  it("filters the Alerts page to the rig", () => {
    expect(rigAlertsLink("rig fra")).toBe("/alerts?rig=rig+fra");
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
