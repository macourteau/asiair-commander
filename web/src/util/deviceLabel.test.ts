import { describe, expect, it } from "vitest";
import {
  deviceFacts,
  deviceLabel,
  rigNamePrefill,
  shortGuid,
  slug,
} from "./deviceLabel";

describe("shortGuid", () => {
  it("trims to eight characters", () => {
    expect(shortGuid("5eed0001000000")).toBe("5eed0001");
  });

  it("says unknown rather than rendering an empty label", () => {
    expect(shortGuid("")).toBe("unknown");
  });
});

describe("deviceLabel", () => {
  it("pairs the model with a short guid", () => {
    expect(deviceLabel({ guid: "5eed00010000", model: "AirPlus", ssid: "" })).toBe(
      "AirPlus (5eed0001)",
    );
  });

  it("falls back to a bare ASIAIR label when the reply carries no model", () => {
    expect(deviceLabel({ guid: "5eed00010000", model: "", ssid: "" })).toBe(
      "ASIAIR 5eed0001",
    );
  });
});

describe("rigNamePrefill", () => {
  it("prefers the ssid, the only field that tells same-model kits apart", () => {
    // Several ASIAIRs of one model report a byte-identical model string; only
    // the ssid, which the operator also sees on their phone, tells them apart.
    const d = {
      guid: "5eed0001",
      model: "Raspberry Pi Compute Module 4 Rev 1.0",
      ssid: "MyRig",
    };
    expect(rigNamePrefill(d)).toBe("MyRig");
    expect(deviceLabel(d)).toBe("Raspberry Pi Compute Module 4 Rev 1.0 (5eed0001)");
  });

  it("falls back to the model label when the device reports no ssid", () => {
    expect(
      rigNamePrefill({ guid: "0a1b2c3d", model: "ZWO AirPlus-RK3568 (Linux)", ssid: "" }),
    ).toBe("ZWO AirPlus-RK3568 (Linux) (0a1b2c3d)");
  });

  it("ignores a whitespace-only ssid", () => {
    expect(rigNamePrefill({ guid: "0a1b2c3d", model: "AirPlus", ssid: "   " })).toBe(
      "AirPlus (0a1b2c3d)",
    );
  });

  it("falls back when the ssid would slug to an empty rig id", () => {
    // The rig id derives from the name, so an all-punctuation ssid would hand
    // the operator a blank id and a validation error instead of a head start.
    expect(rigNamePrefill({ guid: "0a1b2c3d", model: "AirPlus", ssid: "!!!" })).toBe(
      "AirPlus (0a1b2c3d)",
    );
  });

  it("keeps an ssid that slugs to something usable", () => {
    const d = { guid: "0a1b2c3d", model: "AirPlus", ssid: "ASIAIR_0a1b2c3d" };
    expect(rigNamePrefill(d)).toBe("ASIAIR_0a1b2c3d");
    expect(slug(rigNamePrefill(d))).toBe("asiair-0a1b2c3d");
  });
});

describe("slug", () => {
  it("lowercases and joins runs of punctuation with a single dash", () => {
    expect(slug("Backyard 8-inch")).toBe("backyard-8-inch");
    expect(slug("MyRig")).toBe("myrig");
  });

  it("trims leading and trailing dashes", () => {
    expect(slug("  RC8!  ")).toBe("rc8");
  });

  it("returns empty for a name with nothing sluggable in it", () => {
    expect(slug("!!!")).toBe("");
  });
});

describe("deviceFacts", () => {
  it("joins ssid, address and model into one identity strip", () => {
    expect(
      deviceFacts({
        ssid: "MyRig",
        address: "192.168.1.130",
        model: "Raspberry Pi Compute Module 4 Rev 1.0",
      }),
    ).toBe("MyRig · 192.168.1.130 · Raspberry Pi Compute Module 4 Rev 1.0");
  });

  it("drops blank parts rather than leaving stray separators", () => {
    expect(deviceFacts({ ssid: "", address: "192.168.1.130", model: "" })).toBe(
      "192.168.1.130",
    );
    expect(deviceFacts({})).toBe("");
  });
});
