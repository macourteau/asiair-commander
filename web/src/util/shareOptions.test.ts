import { describe, expect, it } from "vitest";
import { probeOutcome, shareOptions } from "./shareOptions";

describe("shareOptions", () => {
  it("offers what the device listed, in the order it listed them", () => {
    expect(shareOptions(["Udisk Images", "EMMC Images"], [])).toEqual([
      "Udisk Images",
      "EMMC Images",
    ]);
  });

  it("keeps a hand-typed share the probe never returned", () => {
    // A rig can be configured while it is switched off, and the probe can fail
    // outright, so the form's own names must survive into the list.
    expect(shareOptions(["Udisk Images"], ["My Weird Share"])).toEqual([
      "Udisk Images",
      "My Weird Share",
    ]);
  });

  it("is the form's names alone when no probe has run", () => {
    expect(shareOptions([], ["Udisk Images", "EMMC Images"])).toEqual([
      "Udisk Images",
      "EMMC Images",
    ]);
    expect(shareOptions([], [])).toEqual([]);
  });

  it("treats a share spelled two ways as one, keeping the device's spelling", () => {
    // SMB share names are case-insensitive, so offering both spellings is noise;
    // the device is the authority on how its own share is written.
    expect(shareOptions(["Udisk Images"], ["udisk images"])).toEqual(["Udisk Images"]);
  });

  it("drops blanks, so an untouched empty row adds no empty option", () => {
    expect(shareOptions(["Udisk Images"], ["", "   "])).toEqual(["Udisk Images"]);
  });

  it("trims surrounding whitespace off the names it offers", () => {
    expect(shareOptions([" Udisk Images "], [])).toEqual(["Udisk Images"]);
  });

  it("does not repeat a share the probe returned twice", () => {
    expect(shareOptions(["Udisk Images", "Udisk Images"], [])).toEqual(["Udisk Images"]);
  });
});

describe("probeOutcome", () => {
  it("counts what was found", () => {
    expect(probeOutcome(["Udisk Images", "EMMC Images"])).toBe(
      "Found 2 shares on this device.",
    );
    expect(probeOutcome(["Udisk Images"])).toBe("Found 1 share on this device.");
  });

  it("says what to do when the device answers with nothing", () => {
    expect(probeOutcome([])).toBe(
      "That device answered but listed no shares. Type the share name by hand.",
    );
  });
});
