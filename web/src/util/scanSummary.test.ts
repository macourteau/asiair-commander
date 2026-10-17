import { describe, expect, it } from "vitest";
import { scanSummary } from "./scanSummary";
import type { DiscoveredRig, RigProfile } from "@/api";

/** device builds a discovered entry; online/managed/rig_id/address drive the summary. */
function device(over: Partial<DiscoveredRig>): DiscoveredRig {
  return {
    guid: "0a1b2c3d",
    cpuId: "0011223344556677",
    model: "ZWO AirPlus-RK3568 (Linux)",
    ssid: "ASIAIR_0a1b2c3d",
    address: "192.168.1.83",
    shares: [],
    online: true,
    managed: false,
    rig_id: "",
    first_seen: 0,
    ...over,
  };
}

/** profile builds the sliver of a rig profile the summary reads: id and name. */
function profile(id: string, name: string): RigProfile {
  return {
    id,
    name,
    guid: "",
    cpuId: "",
    host: "",
    user: "",
    ota: "",
    session: "",
    sources: [],
    managed: true,
    capacity_constrained: false,
    pull_logs: false,
    keep_originals: false,
    sync_clock: false,
  };
}

describe("scanSummary", () => {
  it("counts only the devices still answering, not the whole registry", () => {
    // A managed rig that answered earlier and was then powered off stays in
    // the registry, so the summary counts only the devices that answered this
    // scan, not every entry in the list.
    const found = [
      device({ guid: "live", online: true, managed: true, rig_id: "rig-sharpstar" }),
      device({ guid: "poweredoff", online: false, managed: true, rig_id: "rig-fra" }),
    ];
    expect(scanSummary(found, [profile("rig-sharpstar", "Sharpstar 94")])).toBe(
      "Scan finished — 1 device answered: Sharpstar 94 is reconnecting.",
    );
  });

  it("reports nothing answered when the registry holds only stale entries", () => {
    const found = [
      device({ guid: "a", online: false, managed: true }),
      device({ guid: "b", online: false, managed: false }),
    ];
    expect(scanSummary(found)).toBe("Scan finished — no devices answered.");
  });

  it("reports nothing answered for an empty registry", () => {
    expect(scanSummary([])).toBe("Scan finished — no devices answered.");
  });

  it("names how many answering devices are not set up yet", () => {
    const found = [
      device({ guid: "a", address: "10.0.0.1", online: true, managed: true, rig_id: "rig-1" }),
      device({ guid: "b", address: "10.0.0.2", online: true, managed: false }),
      device({ guid: "c", address: "10.0.0.3", online: true, managed: false }),
      device({ guid: "d", online: false, managed: false }), // stale: excluded from both counts
    ];
    expect(scanSummary(found, [profile("rig-1", "FRA300")])).toBe(
      "Scan finished — 3 devices answered: FRA300 is reconnecting, " +
        "10.0.0.2 and 10.0.0.3 aren't set up yet.",
    );
  });

  it("pluralizes a single answering device", () => {
    expect(scanSummary([device({ address: "10.0.0.9", online: true, managed: false })])).toBe(
      "Scan finished — 1 device answered: 10.0.0.9 isn't set up yet.",
    );
  });

  // Saying "all set up" for a rig whose card still reads Offline points the
  // operator at their network. An answering managed device is reconnecting, and
  // it is named, so the toast and the card can be read against each other.
  it("says an answering managed rig is reconnecting, never that it is all set up", () => {
    const found = [device({ guid: "fra", online: true, managed: true, rig_id: "rig-fra" })];
    const summary = scanSummary(found, [profile("rig-fra", "Sharpstar 94")]);
    expect(summary).toBe("Scan finished — 1 device answered: Sharpstar 94 is reconnecting.");
    expect(summary).not.toContain("set up");
  });

  it("names two reconnecting rigs together", () => {
    const found = [
      device({ guid: "a", online: true, managed: true, rig_id: "rig-1" }),
      device({ guid: "b", online: true, managed: true, rig_id: "rig-2" }),
    ];
    expect(scanSummary(found, [profile("rig-1", "FRA300"), profile("rig-2", "Sharpstar 94")])).toBe(
      "Scan finished — 2 devices answered: FRA300 and Sharpstar 94 are reconnecting.",
    );
  });

  it("falls back to the address when the rig list hasn't loaded", () => {
    const found = [device({ address: "192.168.1.130", online: true, managed: true, rig_id: "rig-1" })];
    expect(scanSummary(found)).toBe(
      "Scan finished — 1 device answered: 192.168.1.130 is reconnecting.",
    );
  });

  // A rig is bound by guid OR by cpuId, so a device that changed guid can answer
  // alongside the registry entry it replaced and both carry the same rig's name.
  // Naming it twice would read as two rigs.
  it("names a rig once even when two answering entries bind to it", () => {
    const found = [
      device({ guid: "old", online: true, managed: true, rig_id: "rig-1" }),
      device({ guid: "rebadged", online: true, managed: true, rig_id: "rig-1" }),
    ];
    expect(scanSummary(found, [profile("rig-1", "FRA300")])).toBe(
      "Scan finished — 2 devices answered: FRA300 is reconnecting.",
    );
  });

  it("drops the names once too many answer to read in one line", () => {
    const found = [
      device({ guid: "a", online: true, managed: true, rig_id: "rig-1" }),
      device({ guid: "b", online: true, managed: true, rig_id: "rig-2" }),
      device({ guid: "c", online: true, managed: false }),
      device({ guid: "d", online: true, managed: false }),
      device({ guid: "e", online: true, managed: false }),
      device({ guid: "f", online: true, managed: false }),
    ];
    expect(scanSummary(found, [profile("rig-1", "FRA300"), profile("rig-2", "Sharpstar 94")])).toBe(
      "Scan finished — 6 devices answered: 2 reconnecting, 4 not set up yet.",
    );
  });
});
