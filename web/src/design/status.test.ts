import { describe, expect, it } from "vitest";
import {
  frameStateView,
  rigStateView,
  safeToPowerOffView,
  showMonitoringPill,
} from "./status";

describe("rigStateView", () => {
  it("appends a pending note only while the requested state disagrees with the published one", () => {
    expect(rigStateView("deleting", "paused").label).toBe("Cleaning up · pausing");
    expect(rigStateView("pulling", "paused").label).toBe("Transferring · pausing");
    expect(rigStateView("paused", "running").label).toBe("Paused · resuming");
    expect(rigStateView("idle", "running", true).label).toBe(
      "Running until idle · resuming",
    );
    expect(rigStateView("paused", "run_until_idle").label).toBe(
      "Paused · starting run",
    );
    expect(rigStateView("idle", "paused", true).label).toBe(
      "Running until idle · stopping",
    );
  });

  it("shows the run label with no note once the request agrees", () => {
    expect(rigStateView("deleting", "run_until_idle", true).label).toBe(
      "Running until idle",
    );
  });

  it("drops the pending note the instant a Resume overtakes a queued pause", () => {
    // Requested flips back to "running" mid-slice; the worker hasn't cycled yet,
    // so state is still "deleting" — no note, straight back to "Cleaning up".
    expect(rigStateView("deleting", "running").label).toBe("Cleaning up");
  });

  it("shows no note once the requested and published states agree, for every base state", () => {
    expect(rigStateView("idle", "running").label).toBe("Monitoring");
    expect(rigStateView("pulling", "running").label).toBe("Transferring");
    expect(rigStateView("deleting", "running").label).toBe("Cleaning up");
    expect(rigStateView("paused", "paused").label).toBe("Paused");
  });

  it("never adds a note on an offline or error rig", () => {
    expect(rigStateView("offline", "paused").label).toBe("Offline");
    expect(rigStateView("error", "paused").label).toBe("Error");
    expect(rigStateView("offline", "running").label).toBe("Offline");
  });
});

describe("showMonitoringPill", () => {
  it("shows the ring only on a genuinely idle, unattended, non-draining rig", () => {
    expect(showMonitoringPill("idle", "running")).toBe(true);
  });

  it("hides the ring behind a pending note", () => {
    expect(showMonitoringPill("idle", "paused")).toBe(false);
  });

  it("hides the ring while a run is in progress", () => {
    expect(showMonitoringPill("idle", "run_until_idle", true)).toBe(false);
  });

  it("hides the ring while a delete backlog is draining", () => {
    expect(showMonitoringPill("idle", "running", false, true)).toBe(false);
  });

  it("hides the ring outside the idle state", () => {
    expect(showMonitoringPill("pulling", "running")).toBe(false);
  });
});

describe("safeToPowerOffView — offline_during_run", () => {
  it("names the run as the cause instead of the generic unknown label", () => {
    const view = safeToPowerOffView("unknown", "offline_during_run");
    expect(view.label).toBe("Unknown — the rig went offline during the last run");
    expect(view.tone).toBe("neutral");
  });

  it("keeps the generic label for every other unknown reason", () => {
    expect(safeToPowerOffView("unknown", "paused").label).toBe(
      "Unknown — don't rely on this",
    );
  });
});

describe("safeToPowerOffView — remote", () => {
  it("reads Unknown and names the remote, not the rig, as what is down", () => {
    const view = safeToPowerOffView("unknown", "remote");
    expect(view.label).toBe("Unknown — don't rely on this");
    expect(view.help).toMatch(/remote this rig is reached through isn't connected/);
  });
});

describe("frameStateView", () => {
  // A frame whose device original was removed must never read as a lost photo:
  // every post-verification label leads with the archive, then says where the
  // device original stands.
  it("labels verified frames by the archive, not the deletion", () => {
    expect(frameStateView("verified").label).toBe("Archived · on device");
    expect(frameStateView("verified", { onHold: true }).label).toBe(
      "Archived · kept on device",
    );
    expect(frameStateView("deleted").label).toBe("Archived · removed from device");
    expect(frameStateView("deleted").tone).toBe("safe");
  });
});
