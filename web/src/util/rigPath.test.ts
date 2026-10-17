import { describe, expect, it } from "vitest";

import { otaRenameNote, rigFolderPreview } from "./rigPath";

const SHIPPED_LIGHT = "<TARGET>/<OTA>/<SESSION>/NIGHT_<n>";

describe("rigFolderPreview", () => {
  it("fills the rig-derived tokens and leaves the frame-derived ones standing", () => {
    expect(
      rigFolderPreview(SHIPPED_LIGHT, {
        ota: "50P",
        session: "Summer2026",
        rigId: "fra300",
      }),
    ).toBe("<TARGET>/50P/Summer2026/NIGHT_<n>");
  });

  it("resolves <RIG> from the id, which a rename never changes", () => {
    expect(
      rigFolderPreview("<RIG>/<TARGET>/NIGHT_<n>", {
        ota: "50P",
        session: "S",
        rigId: "fra300",
      }),
    ).toBe("fra300/<TARGET>/NIGHT_<n>");
  });

  it("keeps a blank value's token rather than collapsing the segment", () => {
    expect(
      rigFolderPreview(SHIPPED_LIGHT, {
        ota: "  ",
        session: "Summer2026",
        rigId: "fra300",
      }),
    ).toBe("<TARGET>/<OTA>/Summer2026/NIGHT_<n>");
  });

  it("normalises a backslash-authored template", () => {
    expect(
      rigFolderPreview("<TARGET>\\<OTA>\\NIGHT_<n>", {
        ota: "50P",
        session: "S",
        rigId: "r",
      }),
    ).toBe("<TARGET>/50P/NIGHT_<n>");
  });

  it("trims the values it substitutes", () => {
    expect(
      rigFolderPreview("<OTA>/<SESSION>", {
        ota: " 50P ",
        session: " S ",
        rigId: "r",
      }),
    ).toBe("50P/S");
  });

  it("renders every occurrence of a repeated token", () => {
    expect(
      rigFolderPreview("<OTA>/x/<OTA>", {
        ota: "50P",
        session: "S",
        rigId: "r",
      }),
    ).toBe("50P/x/50P");
  });

  it("is empty for a blank template", () => {
    expect(
      rigFolderPreview("   ", { ota: "50P", session: "S", rigId: "r" }),
    ).toBe("");
  });
});

describe("otaRenameNote", () => {
  const rig = (name: string, ota: string) => ({ name, ota });

  it("offers the new name when the OTA spells the old one", () => {
    expect(
      otaRenameNote(rig("FRA300", "FRA300"), rig("50P", "FRA300")),
    ).toEqual({
      suggestion: "50P",
    });
  });

  it("ignores case and separators when matching the old name", () => {
    expect(
      otaRenameNote(rig("FRA 300", "fra-300"), rig("50P", "fra-300")),
    ).toEqual({
      suggestion: "50P",
    });
  });

  it("notes the rename without offering a value when the name is more than the scope", () => {
    expect(
      otaRenameNote(rig("FRA300 Rig", "FRA300"), rig("50P Rig", "FRA300")),
    ).toEqual({
      suggestion: null,
    });
  });

  it("notes a rename that has nothing to do with the scope", () => {
    expect(
      otaRenameNote(rig("Backyard rig", "FRA300"), rig("Patio rig", "FRA300")),
    ).toEqual({
      suggestion: null,
    });
  });

  it("says nothing once the OTA is being edited in the same pass", () => {
    expect(
      otaRenameNote(rig("FRA300", "FRA300"), rig("50P", "50P")),
    ).toBeNull();
  });

  it("says nothing when the name has not changed", () => {
    expect(
      otaRenameNote(rig("FRA300", "FRA300"), rig("FRA300", "FRA300")),
    ).toBeNull();
    expect(
      otaRenameNote(rig("FRA300", "FRA300"), rig(" fra300 ", "FRA300")),
    ).toBeNull();
  });

  it("says nothing when the OTA is blank — the required-field error owns that", () => {
    expect(otaRenameNote(rig("FRA300", ""), rig("50P", ""))).toBeNull();
  });

  it("says nothing when either name is blank", () => {
    expect(otaRenameNote(rig("", "FRA300"), rig("50P", "FRA300"))).toBeNull();
    expect(
      otaRenameNote(rig("FRA300", "FRA300"), rig("   ", "FRA300")),
    ).toBeNull();
  });

  it("offers the new name trimmed, since it is about to become a folder", () => {
    expect(
      otaRenameNote(rig("FRA300", "FRA300"), rig("  50P  ", "FRA300")),
    ).toEqual({
      suggestion: "50P",
    });
  });
});
