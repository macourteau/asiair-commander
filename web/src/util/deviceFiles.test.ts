import { describe, expect, it } from "vitest";

import type {
  CopyPlan,
  FilesItemLine,
  FilesReasonCode,
  RigProfile,
  RigStatus,
} from "../api/types";
import {
  abortedSuffix,
  ancestorsOf,
  applyRowUpdates,
  baseName,
  conflictDecisions,
  conflictHint,
  copyButtonLabel,
  copyDoneText,
  copyPlanTitle,
  copySelection,
  copyWatchedLines,
  defaultRigs,
  deleteButtonLabel,
  deleteDoneText,
  deleteSelectionLine,
  deleteSummaryLine,
  extSummaryLines,
  IDENTIFYING_REASON,
  initialDecisions,
  isListed,
  isNotAttempted,
  isSameOrInside,
  isUnknownRig,
  joinPath,
  listingRegionLabel,
  mergeRunReport,
  overwriteConfirmText,
  overwriteNeedsConfirm,
  paneRigNotice,
  parentDir,
  pathSegments,
  readPanePlace,
  reasonText,
  reportCounts,
  rigNamedIn,
  rigOptions,
  rowOutcomeLabel,
  rowRunAnnouncement,
  samePath,
  SELECT_FIRST_REASON,
  selectionText,
  settledRows,
  toolbarState,
  BUSY_REASON,
  leaveRunningText,
  nonJsonUploadRefusal,
  placeText,
  planFailedText,
  PROGRESS_LABEL,
  renameDoneText,
  renameErrorText,
  renameRefusalShown,
  renameSelection,
  RUNNING_ROW_LABEL,
  sendFailedText,
  sendStoppedText,
  sendingNote,
  sendingText,
  UPLOAD_NAME_MAX_BYTES,
  uploadCheckingText,
  uploadDoneText,
  uploadPickProblem,
  uploadSkipReason,
  uploadPlanTitle,
  uploadReportTitle,
  uploadSpaceWarning,
  uploadTypeConflictText,
  uploadWritingText,
  validateName,
  WATCHED_RENAME_NOTE,
  watchedFolderNotice,
  waitingText,
  WAITING_AFTER_MS,
  writePanePlace,
  type ReasonContext,
  type ToolbarPane,
} from "./deviceFiles";

/* ---- paths ---------------------------------------------------------------- */

describe("path helpers", () => {
  it("joins against the share root without a leading slash", () => {
    expect(joinPath("", "Preview")).toBe("Preview");
    expect(joinPath("Preview", "ref.fit")).toBe("Preview/ref.fit");
    expect(joinPath("Preview", "")).toBe("Preview");
  });

  it("splits parent and base name", () => {
    expect(parentDir("a/b/c.fit")).toBe("a/b");
    expect(parentDir("c.fit")).toBe("");
    expect(baseName("a/b/c.fit")).toBe("c.fit");
    expect(baseName("c.fit")).toBe("c.fit");
  });

  it("gives every breadcrumb segment the path it opens", () => {
    expect(pathSegments("")).toEqual([]);
    expect(pathSegments("ASIAIR/Autorun/Light")).toEqual([
      { name: "ASIAIR", path: "ASIAIR" },
      { name: "Autorun", path: "ASIAIR/Autorun" },
      { name: "Light", path: "ASIAIR/Autorun/Light" },
    ]);
  });

  it("compares paths ignoring case, as FAT does", () => {
    expect(samePath("Preview", "PREVIEW")).toBe(true);
    expect(isSameOrInside("preview/M42", "Preview")).toBe(true);
    expect(isSameOrInside("Preview", "Preview")).toBe(true);
    expect(isSameOrInside("Previews", "Preview")).toBe(false);
    expect(isSameOrInside("anything", "")).toBe(true);
  });

  it("lists a path's ancestors, root first", () => {
    expect(ancestorsOf("a/b/c.fit")).toEqual(["a", "a/b"]);
    expect(ancestorsOf("c.fit")).toEqual([]);
  });
});

describe("pane place in the query string", () => {
  it("round-trips a folder", () => {
    const p = writePanePlace(new URLSearchParams(), "left", {
      rig: "r1",
      share: "EMMC Images",
      dir: "Preview/M42",
    });
    expect(readPanePlace(p, "left")).toEqual({
      rig: "r1",
      share: "EMMC Images",
      dir: "Preview/M42",
    });
    expect(readPanePlace(p, "right")).toBeNull();
  });

  it("keeps the share list (no share) apart from a share root", () => {
    const list = writePanePlace(new URLSearchParams(), "right", { rig: "r2", share: null, dir: "" });
    expect(readPanePlace(list, "right")).toEqual({ rig: "r2", share: null, dir: "" });
    const root = writePanePlace(list, "right", { rig: "r2", share: "Udisk Images", dir: "" });
    expect(readPanePlace(root, "right")).toEqual({ rig: "r2", share: "Udisk Images", dir: "" });
  });

  it("records an explicit no-rig choice so the default does not return", () => {
    const p = writePanePlace(new URLSearchParams("lrig=r1&lshare=x"), "left", {
      rig: null,
      share: null,
      dir: "",
    });
    expect(p.has("lrig")).toBe(true);
    expect(p.has("lshare")).toBe(false);
    expect(readPanePlace(p, "left")).toEqual({ rig: null, share: null, dir: "" });
  });
});

/* ---- rig selector --------------------------------------------------------- */

function status(over: Partial<RigStatus>): RigStatus {
  return {
    id: "r1",
    name: "Rig One",
    state: "idle",
    online: true,
    paused: false,
    requested: "running",
    frames_tonight: 0,
    shares: [],
    device_frames: null,
    keep_originals: false,
    safe_to_power_off: "safe",
    listing_ts: 0,
    ...over,
  };
}

describe("rig selector", () => {
  const fleet = [
    status({ id: "a", name: "Alpha", online: false }),
    status({ id: "b", name: "Bravo", remote_name: "Cabin" }),
    status({ id: "c", name: "Charlie" }),
  ];

  it("labels offline and remote rigs, and disables offline ones", () => {
    expect(rigOptions(fleet)).toEqual([
      { value: "a", label: "Alpha (offline)", disabled: true },
      { value: "b", label: "Bravo (via Cabin)", disabled: false },
      { value: "c", label: "Charlie", disabled: false },
    ]);
  });

  it("defaults Left to the first online rig and Right to the second", () => {
    expect(defaultRigs(fleet)).toEqual(["b", "c"]);
    expect(defaultRigs([fleet[1]!])).toEqual(["b", null]);
    expect(defaultRigs([fleet[0]!])).toEqual([null, null]);
  });

  it("explains a selected rig that went offline or left the managed set", () => {
    const profiles = [
      { id: "u", name: "Unmanaged", managed: false },
      { id: "b", name: "Bravo", managed: true },
    ] as RigProfile[];
    expect(paneRigNotice("a", fleet, profiles)).toBe("Alpha is offline");
    expect(paneRigNotice("b", fleet, profiles)).toBeNull();
    expect(paneRigNotice("u", fleet, profiles)).toBe("Unmanaged is no longer managed");
    expect(paneRigNotice("gone", fleet, profiles)).toBe("gone is not one of your managed rigs");
    expect(paneRigNotice("gone", fleet, null)).toBeNull();
  });

  it("treats a rig neither in the fleet nor configured as unknown, once profiles load", () => {
    const profiles = [{ id: "u", name: "Unmanaged", managed: false }] as RigProfile[];
    expect(isUnknownRig("gone", fleet, profiles)).toBe(true);
    expect(isUnknownRig("gone", fleet, null)).toBe(false);
    expect(isUnknownRig("u", fleet, profiles)).toBe(false);
    expect(isUnknownRig("a", fleet, [])).toBe(false);
  });

  it("names a pane's listing by side, rig and place", () => {
    expect(listingRegionLabel("left", "Alpha", "EMMC Images", "Autorun/Light")).toBe(
      "Left pane: Alpha › EMMC Images › Autorun › Light",
    );
    expect(listingRegionLabel("right", "Bravo", null, "")).toBe("Right pane: Bravo");
    expect(listingRegionLabel("right", "", null, "")).toBe("Right pane");
  });
});

/* ---- toolbar -------------------------------------------------------------- */

function pane(over: Partial<ToolbarPane>): ToolbarPane {
  return {
    rig: "a",
    rigName: "Alpha",
    share: "EMMC Images",
    dir: "Preview",
    writable: true,
    notFound: false,
    selected: [{ name: "ref.fit", dir: false }],
    shares: ["EMMC Images", "Udisk Images"],
    ...over,
  };
}

describe("toolbarState", () => {
  it("enables every action for two writable rigs in different folders", () => {
    const t = toolbarState(pane({}), pane({ rig: "b", rigName: "Bravo", selected: [] }), false);
    expect(t.left.download.enabled).toBe(true);
    expect(t.left.copyOther.enabled).toBe(true);
    expect(t.left.copySame.enabled).toBe(true);
    expect(t.left.newFolder.enabled).toBe(true);
    expect(t.left.upload.enabled).toBe(true);
    expect(t.left.rename.enabled).toBe(true);
    expect(t.left.delete.enabled).toBe(true);
    expect(t.left.copyOtherLabel).toBe("Copy to Bravo");
    expect(t.right.copyOtherLabel).toBe("Copy to Alpha");
    // One rig in both panes: the destination is a folder, not a rig.
    const same = toolbarState(pane({}), pane({ dir: "Other" }), false);
    expect(same.left.copyOtherLabel).toBe("Copy to other folder");
    expect(t.left.paneReasons).toEqual([]);
  });

  it("needs a selection for everything but New folder and Upload", () => {
    const t = toolbarState(pane({ selected: [] }), pane({ rig: "b" }), false);
    expect(t.left.download.enabled).toBe(false);
    expect(t.left.download.reason).toMatch(/Select/);
    expect(t.left.copyOther.enabled).toBe(false);
    expect(t.left.copySame.enabled).toBe(false);
    expect(t.left.delete.enabled).toBe(false);
    expect(t.left.rename).toEqual({ enabled: false, reason: "Select one file or folder to rename." });
    expect(t.left.newFolder.enabled).toBe(true);
    expect(t.left.upload.enabled).toBe(true);
    // Said on a visible line too, which a keyboard user reaches in reading order.
    expect(t.left.paneReasons).toEqual([SELECT_FIRST_REASON]);
  });

  it("says nothing about selecting at the share list, in a missing folder or while busy", () => {
    const other = pane({ rig: "b" });
    expect(toolbarState(pane({ share: null, selected: [] }), other, false).left.paneReasons).toEqual([]);
    expect(toolbarState(pane({ notFound: true, selected: [] }), other, false).left.paneReasons).toEqual([]);
    expect(toolbarState(pane({ selected: [] }), other, true).left.paneReasons).toEqual([]);
  });

  it("disables everything at the share list", () => {
    const t = toolbarState(pane({ share: null, dir: "", selected: [] }), pane({ rig: "b" }), false);
    for (const b of [t.left.download, t.left.copyOther, t.left.newFolder, t.left.upload, t.left.rename, t.left.delete]) {
      expect(b.enabled).toBe(false);
    }
    expect(t.left.upload.reason).toBe("Open a share first.");
    expect(t.left.rename.reason).toBe("Open a share first.");
  });

  it("gives Upload and Rename New folder's reasons without a rig", () => {
    const t = toolbarState(pane({ rig: null, rigName: "", share: null, selected: [] }), pane({ rig: "b" }), false);
    expect(t.left.upload).toEqual({ enabled: false, reason: "Choose a rig first." });
    expect(t.left.rename).toEqual({ enabled: false, reason: "Choose a rig first." });
  });

  it("disables New folder and every selection button for a not_found pane", () => {
    const t = toolbarState(pane({ notFound: true }), pane({ rig: "b" }), false);
    expect(t.left.upload).toEqual({ enabled: false, reason: "This folder no longer exists." });
    expect(t.left.rename).toEqual({ enabled: false, reason: "This folder no longer exists." });
    expect(t.left.newFolder.enabled).toBe(false);
    expect(t.left.download.enabled).toBe(false);
    expect(t.left.copyOther.enabled).toBe(false);
    expect(t.left.copySame.enabled).toBe(false);
    expect(t.left.delete.enabled).toBe(false);
  });

  it("keeps reads but refuses writes on a rig not yet identified, with a visible reason", () => {
    const t = toolbarState(pane({ writable: false }), pane({ rig: "b" }), false);
    expect(t.left.download.enabled).toBe(true);
    expect(t.left.copyOther.enabled).toBe(true);
    expect(t.left.newFolder.enabled).toBe(false);
    expect(t.left.upload).toEqual({ enabled: false, reason: IDENTIFYING_REASON });
    expect(t.left.rename).toEqual({ enabled: false, reason: IDENTIFYING_REASON });
    expect(t.left.delete.enabled).toBe(false);
    expect(t.left.delete.reason).toBe(IDENTIFYING_REASON);
    expect(t.left.paneReasons).toContain(IDENTIFYING_REASON);
    // Nothing can be copied into it from the other pane either.
    expect(t.right.copyOther.enabled).toBe(false);
    expect(t.right.copySame.enabled).toBe(false);
  });

  it("disables copies when the other pane has no rig, saying so under the toolbar", () => {
    const t = toolbarState(pane({}), pane({ rig: null, rigName: "", share: null }), false);
    expect(t.left.copyOther.enabled).toBe(false);
    expect(t.left.copySame.enabled).toBe(false);
    expect(t.left.download.enabled).toBe(true);
    expect(t.left.paneReasons).toEqual(["Choose a rig in the right pane to copy there."]);
    expect(t.left.copyOtherLabel).toBe("Copy to other pane");
  });

  it("needs the other pane inside a share and its folder present", () => {
    expect(
      toolbarState(pane({}), pane({ rig: "b", share: null }), false).left.copyOther.enabled,
    ).toBe(false);
    expect(
      toolbarState(pane({}), pane({ rig: "b", notFound: true }), false).left.copyOther.enabled,
    ).toBe(false);
  });

  it("refuses a copy into the same folder and into a selected folder", () => {
    const same = toolbarState(pane({}), pane({}), false);
    expect(same.left.copyOther.enabled).toBe(false);
    expect(same.left.copyOther.reason).toMatch(/same folder/);

    const into = toolbarState(
      pane({ selected: [{ name: "M42", dir: true }] }),
      pane({ dir: "preview/m42/flats" }),
      false,
    );
    expect(into.left.copyOther.enabled).toBe(false);
    expect(into.left.copyOther.reason).toMatch(/into itself/);

    // The same folder name on another share is somewhere else.
    const otherShare = toolbarState(
      pane({ selected: [{ name: "M42", dir: true }] }),
      pane({ share: "Udisk Images", dir: "Preview/M42" }),
      false,
    );
    expect(otherShare.left.copyOther.enabled).toBe(true);

    // A sibling folder of the same rig is a fine destination.
    const sibling = toolbarState(pane({}), pane({ dir: "Preview/M31" }), false);
    expect(sibling.left.copyOther.enabled).toBe(true);
  });

  it("allows Copy to same location only to a different rig that has the share", () => {
    // Same rig in both panes.
    expect(toolbarState(pane({}), pane({ selected: [] }), false).left.copySame.enabled).toBe(false);
    const bravo = { rig: "b", rigName: "Bravo", shares: ["EMMC Images"] };
    expect(toolbarState(pane({}), pane(bravo), false).left.copySame.enabled).toBe(true);
    const missing = toolbarState(pane({ share: "Udisk Images" }), pane(bravo), false);
    expect(missing.left.copySame.enabled).toBe(false);
    expect(missing.left.copySame.reason).toBe("Bravo has no share named Udisk Images.");
    // The other rig's share list is not known yet.
    expect(
      toolbarState(pane({}), pane({ ...bravo, shares: null }), false).left.copySame.enabled,
    ).toBe(false);
  });

  it("gives Copy to same location a neutral reason while the other rig is still loading", () => {
    const bravo = { rig: "b", rigName: "Bravo", shares: ["EMMC Images"] };
    const loading = { enabled: false, reason: "Waiting for Bravo's folders to load." };
    // Its share list or its first listing has not answered: neither "no share
    // named…" nor "still identifying" is known yet.
    expect(toolbarState(pane({}), pane({ ...bravo, shares: null }), false).left.copySame).toEqual(loading);
    expect(
      toolbarState(pane({}), pane({ ...bravo, shares: null, writable: null }), false).left.copySame,
    ).toEqual(loading);
    expect(toolbarState(pane({}), pane({ ...bravo, writable: null }), false).left.copySame).toEqual(loading);
    // A listing that answered read-only still says why.
    expect(
      toolbarState(pane({}), pane({ ...bravo, shares: null, writable: false }), false).left.copySame.reason,
    ).toBe(`Bravo: ${IDENTIFYING_REASON}.`);
  });

  it("disables every button while an operation runs", () => {
    const t = toolbarState(pane({}), pane({ rig: "b" }), true);
    for (const b of [
      t.left.download,
      t.left.copyOther,
      t.left.copySame,
      t.left.upload,
      t.left.newFolder,
      t.left.rename,
      t.left.delete,
    ]) {
      expect(b).toEqual({ enabled: false, reason: BUSY_REASON });
    }
    expect(BUSY_REASON).toBe("Another copy, upload, rename, delete or download is running.");
  });

  it("asks to select first for every selection action, rename included", () => {
    expect(SELECT_FIRST_REASON).toBe("Select files or folders to download, copy, rename or delete.");
  });

  it("leaves rename out of the select-first line in a watched folder", () => {
    const t = toolbarState(pane({ watched: true, selected: [] }), pane({ rig: "b" }), false);
    expect(t.left.paneReasons).toEqual(["Select files or folders to download, copy or delete."]);
  });

  it("renames one item at a time", () => {
    const t = toolbarState(
      pane({ selected: [{ name: "a.fit", dir: false }, { name: "M42", dir: true }] }),
      pane({ rig: "b" }),
      false,
    );
    expect(t.left.rename).toEqual({ enabled: false, reason: "Rename works on one item at a time." });
    expect(t.left.delete.enabled).toBe(true);
  });

  const r1 = { rule: "watched" as const, message: "Commander archives this folder by name; renaming here would make it archive these frames again." };
  const r2 = { rule: "reserved" as const, message: "Autorun is, or holds, EMMC Images/Autorun, a folder Commander depends on." };
  const unchecked = { rule: "unchecked" as const, message: "Commander couldn't read its records to check renames here. Try again." };

  it("disables Rename for a refused entry with the refusal's sentence as its reason", () => {
    for (const refusal of [r1, r2, unchecked]) {
      const t = toolbarState(
        pane({ selected: [{ name: "x", dir: true, renameRefused: refusal }] }),
        pane({ rig: "b" }),
        false,
      );
      expect(t.left.rename).toEqual({ enabled: false, reason: refusal.message });
    }
  });

  it("says R1 in a watched folder once, in its notice, and every other refusal on a visible line", () => {
    const sel = (refusal: typeof r1 | typeof r2 | typeof unchecked) => [
      { name: "x", dir: false, renameRefused: refusal },
    ];
    // R1 where an enabled source watches the folder: the watched notice says it.
    expect(toolbarState(pane({ watched: true, selected: sel(r1) }), pane({ rig: "b" }), false).left.paneReasons).toEqual([]);
    // R1 caused only by a turned-off source or a root the store keeps in play:
    // no watched notice shows, so the line does.
    expect(toolbarState(pane({ watched: false, selected: sel(r1) }), pane({ rig: "b" }), false).left.paneReasons).toEqual([r1.message]);
    // R2 and an unchecked store read always get the line.
    expect(toolbarState(pane({ watched: true, selected: sel(r2) }), pane({ rig: "b" }), false).left.paneReasons).toEqual([r2.message]);
    expect(toolbarState(pane({ watched: false, selected: sel(unchecked) }), pane({ rig: "b" }), false).left.paneReasons).toEqual([unchecked.message]);
    expect(renameRefusalShown(r1, true)).toBe(false);
    expect(renameRefusalShown(r1, false)).toBe(true);
  });

  it("hands the live region exactly the refusal that has its own line", () => {
    const one = (refusal: typeof r1 | typeof r2, watched: boolean, busy = false) =>
      toolbarState(pane({ watched, selected: [{ name: "x", dir: false, renameRefused: refusal }] }), pane({ rig: "b" }), busy)
        .left.renameRefusal;
    expect(one(r2, false)).toBe(r2.message);
    expect(one(r1, false)).toBe(r1.message);
    // The watched notice says R1 there, once on entering the folder.
    expect(one(r1, true)).toBeNull();
    expect(one(r2, false, true)).toBeNull();
    expect(toolbarState(pane({ selected: [] }), pane({ rig: "b" }), false).left.renameRefusal).toBeNull();
  });

  it("shows the refusal line only while that one item is selected", () => {
    const refused = { name: "x", dir: false, renameRefused: r2 };
    const two = toolbarState(pane({ selected: [refused, { name: "y", dir: false }] }), pane({ rig: "b" }), false);
    expect(two.left.paneReasons).toEqual([]);
    const busy = toolbarState(pane({ selected: [refused] }), pane({ rig: "b" }), true);
    expect(busy.left.paneReasons).toEqual([]);
  });
});

describe("selectionText", () => {
  it("counts items and sums file sizes only", () => {
    expect(selectionText([])).toBe("");
    expect(
      selectionText([
        { dir: false, size: 1024 * 1024 },
        { dir: true, size: 4096 },
      ]),
    ).toBe("2 selected · 1.0 MB");
    expect(selectionText([{ dir: true, size: 0 }])).toBe("1 selected");
  });
});

/* ---- folder names --------------------------------------------------------- */

describe("validateName", () => {
  it("accepts an ordinary name, inner spaces and dots included", () => {
    for (const kind of ["folder", "file", "any"] as const) {
      expect(validateName("M42 2026.10", kind)).toBeNull();
    }
  });

  it.each([
    ["", /Enter/],
    [".", /can't be used/],
    ["..", /can't be used/],
    ["a/b", /can't contain/],
    ["a\\b", /can't contain/],
    ["a:b", /can't contain/],
    ["a*b", /can't contain/],
    ["a?b", /can't contain/],
    ['a"b', /can't contain/],
    ["a<b", /can't contain/],
    ["a>b", /can't contain/],
    ["a|b", /can't contain/],
    ["a\u0001b", /control/],
    ["a\u007fb", /control/],
    ["a\u0085b", /control/],
    ["zero\u200bwidth", /invisible/],
    ["zw\u200cnj", /invisible/],
    ["zw\u200dj", /invisible/],
    ["bidi\u202eover", /invisible/],
    ["iso\u2066late", /invisible/],
    ["\ufeffbom", /invisible/],
    ["soft\u00adhyphen", /invisible/],
    [" leading", /start with a space/],
    ["trailing ", /end with/],
    ["trailing.", /end with/],
    ["x".repeat(256), /255 bytes/],
    ["é".repeat(128), /255 bytes/], // 256 bytes in UTF-8
    [".asiair-commander-media-id", /device-identity/],
    [".ASIAIR-Commander-Rig-ID", /device-identity/],
  ])("rejects %j", (name, why) => {
    for (const kind of ["folder", "file", "any"] as const) {
      expect(validateName(name, kind)).toMatch(why);
    }
  });

  it("words each kind, keeping New folder's messages", () => {
    expect(validateName("", "folder")).toBe("Enter a folder name.");
    expect(validateName("", "file")).toBe("Enter a file name.");
    expect(validateName("", "any")).toBe("Enter a name.");
    expect(validateName("a?b", "folder")).toBe('Folder names can\'t contain / \\ : * ? " < > |.');
    expect(validateName("a?b", "file")).toBe('File names can\'t contain / \\ : * ? " < > |.');
    expect(validateName("a?b", "any")).toBe('Names can\'t contain / \\ : * ? " < > |.');
    expect(validateName("..", "folder")).toBe('".." can\'t be used as a folder name.');
    expect(validateName("x.", "any")).toBe("Names can't end with a space or a dot.");
  });

  it("accepts exactly 255 bytes", () => {
    expect(validateName("x".repeat(255), "any")).toBeNull();
  });
});

describe("uploadPickProblem", () => {
  it("accepts a name the device and the upload cap allow, up to 224 bytes", () => {
    expect(UPLOAD_NAME_MAX_BYTES).toBe(224);
    expect(uploadPickProblem("ref.fit", "Alpha")).toBeNull();
    expect(uploadPickProblem("x".repeat(224), "Alpha")).toBeNull();
  });

  it("refuses a name over 224 bytes, naming the rig", () => {
    const name = "x".repeat(225);
    expect(uploadPickProblem(name, "Alpha")).toBe(
      `${name} can't be uploaded under its current name (name too long for Alpha). Rename it on your computer and choose it again.`,
    );
    // 225 bytes in UTF-8 from fewer characters.
    expect(uploadPickProblem("é".repeat(112) + "x", "Alpha")).toMatch(/name too long for Alpha/);
  });

  it("refuses every name the shared rules refuse, with the fix on the computer", () => {
    expect(uploadPickProblem("ref.", "Alpha")).toBe(
      "ref. can't be uploaded under its current name (ends with a space or a dot). Rename it on your computer and choose it again.",
    );
    for (const name of ["a:b.fit", " lead.fit", "zw\u200b.fit", ".asiair-commander-media-id"]) {
      expect(uploadPickProblem(name, "Alpha")).toMatch(/Rename it on your computer and choose it again\.$/);
    }
  });
});

describe("renameSelection", () => {
  it("selects a file's stem, so typing keeps the extension", () => {
    expect(renameSelection("ref.fit", false)).toEqual([0, 3]);
    expect(renameSelection("Light_M31.2026.fits", false)).toEqual([0, 14]);
  });

  it("selects the whole name of a folder, a dotfile or a name without a dot", () => {
    expect(renameSelection("M42.old", true)).toEqual([0, 7]);
    expect(renameSelection(".hidden", false)).toEqual([0, 7]);
    expect(renameSelection("README", false)).toEqual([0, 6]);
  });
});

/* ---- copy plan ------------------------------------------------------------ */

function plan(over: Partial<CopyPlan> = {}): CopyPlan {
  return {
    files: 5,
    bytes: 500,
    dirs: ["M42", "M42/flats", "M31"],
    items: [
      { rel: "ref.fit", size: 100, watched: false },
      { rel: "ref.jpg", size: 100, watched: false },
      { rel: "M42/a.fit", size: 100, watched: false },
      { rel: "M42/flats/f.fit", size: 100, watched: false },
      { rel: "M31/b.fit", size: 100, watched: false },
    ],
    conflicts: [
      { rel: "ref.fit", kind: "file", src_size: 100, src_mtime: 1, dst_size: 90, dst_mtime: 2, dst_ledger: "unarchived" },
      { rel: "ref.jpg", kind: "file", src_size: 100, src_mtime: 1, dst_size: 80, dst_mtime: 3, dst_ledger: "" },
      { rel: "M42", kind: "type", src_size: 0, src_mtime: 0, dst_size: 5, dst_mtime: 0, dst_ledger: "" },
    ],
    to_dir_missing: 0,
    watched: { frames: 0, previews: 0, keeps_originals: false },
    ...over,
  };
}

describe("conflictDecisions", () => {
  const start = initialDecisions(plan().conflicts);

  it("starts every overwritable conflict at Skip, and ignores type conflicts", () => {
    expect(start).toEqual({ "ref.fit": "skip", "ref.jpg": "skip" });
  });

  it("sets one row", () => {
    expect(conflictDecisions(start, { type: "set", rel: "ref.fit", decision: "overwrite" })).toEqual({
      "ref.fit": "overwrite",
      "ref.jpg": "skip",
    });
  });

  it("sets every row with Overwrite all and Skip all", () => {
    const all = conflictDecisions(start, { type: "all", decision: "overwrite" });
    expect(all).toEqual({ "ref.fit": "overwrite", "ref.jpg": "overwrite" });
    expect(conflictDecisions(all, { type: "all", decision: "skip" })).toEqual(start);
  });

  it("cannot mark a type conflict or an unknown file for overwrite", () => {
    expect(conflictDecisions(start, { type: "set", rel: "M42", decision: "overwrite" })).toBe(start);
    expect(conflictDecisions(start, { type: "set", rel: "nope", decision: "overwrite" })).toBe(start);
  });
});

describe("copySelection", () => {
  it("drops skips and type-conflicted subtrees from what is sent and counted", () => {
    const p = plan();
    const sel = copySelection(p, initialDecisions(p.conflicts));
    expect(sel.items.map((i) => i.rel)).toEqual(["M31/b.fit"]);
    expect(sel.files).toBe(1);
    expect(sel.bytes).toBe(100);
    expect(sel.skipped).toBe(4);
    expect(sel.overwrite).toEqual([]);
    expect(sel.dirs).toEqual(["M31"]);
    expect(sel.createFolders).toBe(1);
    expect(sel.typeConflicts).toEqual([{ rel: "M42", isDir: true, files: 2 }]);
  });

  it("sends the chosen overwrites with the destination state the user saw", () => {
    const p = plan();
    const d = conflictDecisions(initialDecisions(p.conflicts), {
      type: "set",
      rel: "ref.fit",
      decision: "overwrite",
    });
    const sel = copySelection(p, d);
    expect(sel.items.map((i) => i.rel)).toEqual(["ref.fit", "M31/b.fit"]);
    expect(sel.overwrite).toEqual([{ rel: "ref.fit", dst_size: 90, dst_mtime: 2 }]);
    expect(sel.skipped).toBe(3);
  });

  it("counts the missing levels of a mirrored destination as folders to create", () => {
    const sel = copySelection(plan({ conflicts: [], to_dir_missing: 2 }), {});
    expect(sel.createFolders).toBe(5);
  });

  it("names a type-conflicted file (not folder) without a count", () => {
    const p = plan({
      conflicts: [
        { rel: "ref.fit", kind: "type", src_size: 100, src_mtime: 1, dst_size: 0, dst_mtime: 0, dst_ledger: "" },
      ],
    });
    expect(copySelection(p, {}).typeConflicts).toEqual([{ rel: "ref.fit", isDir: false, files: 0 }]);
  });
});

describe("copy plan wording", () => {
  it("titles the plan with what is actually sent", () => {
    const sel = { skipped: 0, createFolders: 0 };
    expect(copyPlanTitle({ ...sel, files: 12, bytes: 1024 ** 3 * 1.1 }, "Bravo", false)).toBe(
      "Copy 12 files (1.1 GB) to Bravo",
    );
    expect(copyPlanTitle({ ...sel, files: 1, bytes: 1024 }, "Bravo", true)).toBe(
      "Copy 1 file (1.0 KB) to the same location on Bravo",
    );
  });

  it("titles a plan that copies nothing by what already exists", () => {
    expect(copyPlanTitle({ files: 0, bytes: 0, skipped: 3, createFolders: 0 }, "Bravo", false)).toBe(
      "Nothing to copy: 3 files already exist on Bravo",
    );
    expect(copyPlanTitle({ files: 0, bytes: 0, skipped: 1, createFolders: 0 }, "Bravo", true)).toBe(
      "Nothing to copy: 1 file already exists on Bravo",
    );
    expect(copyPlanTitle({ files: 0, bytes: 0, skipped: 0, createFolders: 2 }, "Bravo", false)).toBe(
      "Create 2 folders on Bravo",
    );
    expect(copyPlanTitle({ files: 0, bytes: 0, skipped: 0, createFolders: 0 }, "Bravo", false)).toBe(
      "Nothing to copy",
    );
  });

  it("toasts a clean run with what it did, naming the folders a folders-only copy created", () => {
    expect(copyDoneText({ files: 4, bytes: 4000, createFolders: 2 }, "Bravo")).toBe(
      "Copied 4 files (3.9 KB) to Bravo",
    );
    expect(copyDoneText({ files: 0, bytes: 0, createFolders: 2 }, "Bravo")).toBe("Created 2 folders on Bravo");
    expect(copyDoneText({ files: 0, bytes: 0, createFolders: 1 }, "Bravo")).toBe("Created 1 folder on Bravo");
  });

  it("labels the primary button from the decisions", () => {
    expect(copyButtonLabel({ files: 12, skipped: 0, createFolders: 0 })).toBe("Copy 12 files");
    expect(copyButtonLabel({ files: 8, skipped: 4, createFolders: 0 })).toBe("Copy 8 files, skip 4");
    expect(copyButtonLabel({ files: 5, skipped: 0, createFolders: 2 })).toBe(
      "Copy 5 files, create 2 folders",
    );
    expect(copyButtonLabel({ files: 0, skipped: 0, createFolders: 3 })).toBe("Create 3 folders");
    expect(copyButtonLabel({ files: 0, skipped: 2, createFolders: 0 })).toBe("");
  });

  it("says nothing about archiving for an unwatched destination", () => {
    expect(copyWatchedLines({ frames: 0, previews: 0, keeps_originals: false }, "Bravo", false)).toEqual([]);
  });

  it("warns about frames and previews landing in a watched folder", () => {
    expect(copyWatchedLines({ frames: 8, previews: 2, keeps_originals: false }, "Bravo", false)).toEqual([
      "8 frames will be archived as Bravo's frames, filed under its telescope and session, and then removed from Bravo after the grace period.",
      "2 previews (jpg and other excluded file types) are not archived, and Commander may remove them from Bravo.",
    ]);
  });

  it("keeps originals on a keep-originals rig, and says 'up to' once anything is skipped", () => {
    const lines = copyWatchedLines({ frames: 8, previews: 2, keeps_originals: true }, "Bravo", true);
    expect(lines[0]).toMatch(/^Up to 8 frames will be archived/);
    expect(lines[0]).toMatch(/stay on the device/);
    expect(lines[1]).toBe(
      "Up to 2 previews (jpg and other excluded file types) are not archived and stay on Bravo.",
    );
    expect(copyWatchedLines({ frames: 1, previews: 0, keeps_originals: false }, "Bravo", false)).toEqual([
      "1 frame will be archived as one of Bravo's frames, filed under its telescope and session, and then removed from Bravo after the grace period.",
    ]);
    expect(copyWatchedLines({ frames: 1, previews: 0, keeps_originals: true }, "Bravo", false)[0]).toBe(
      "1 frame will be archived as one of Bravo's frames and filed under its telescope and session; Bravo keeps originals, so it stays on the device.",
    );
    expect(copyWatchedLines({ frames: 0, previews: 1, keeps_originals: false }, "Bravo", true)).toEqual([
      "Up to 1 preview (jpg and other excluded file types) is not archived, and Commander may remove them from Bravo.",
    ]);
  });

  it("hints at what overwriting a tracked frame does", () => {
    expect(conflictHint("", "Bravo")).toBeNull();
    expect(conflictHint("unarchived", "Bravo")).toBe("Not archived yet: overwriting loses it.");
    expect(conflictHint("archived", "Bravo")).toMatch(/integrity alert on Bravo/);
  });
});

/* ---- delete plan ---------------------------------------------------------- */

describe("delete plan wording", () => {
  it("lists extensions largest total first, with no-extension files named", () => {
    expect(
      extSummaryLines([
        { ext: "jpg", files: 50, bytes: 103 * 1024 * 1024 },
        { ext: "", files: 4, bytes: 2048 },
        { ext: "fits", files: 10, bytes: 1.1 * 1024 ** 3 },
        { ext: "fit", files: 1, bytes: 10 },
      ]),
    ).toEqual(["10 fits — 1.1 GB", "50 jpg — 103.0 MB", "4 (no extension) — 2.0 KB", "1 fit — 10 B"]);
  });

  it("summarises folders, files and size, or empty folders", () => {
    expect(deleteSummaryLine({ dirs: 3, files: 120, bytes: 1024 ** 3 * 1.2 })).toBe(
      "3 folders, 120 files, 1.2 GB",
    );
    expect(deleteSummaryLine({ dirs: 0, files: 1, bytes: 1024 })).toBe("1 file, 1.0 KB");
    expect(deleteSummaryLine({ dirs: 2, files: 0, bytes: 0 })).toBe("2 empty folders");
  });

  it("labels the danger button from what is deleted", () => {
    expect(deleteButtonLabel({ dirs: 0, files: 120 })).toBe("Delete 120 files");
    expect(deleteButtonLabel({ dirs: 3, files: 120 })).toBe("Delete 120 files and 3 folders");
    expect(deleteButtonLabel({ dirs: 2, files: 0 })).toBe("Delete 2 folders");
  });

  it("words the clean delete's toast like its button", () => {
    expect(deleteDoneText({ dirs: 1, files: 3 }, "Alpha")).toBe("Deleted 3 files and 1 folder from Alpha");
    expect(deleteDoneText({ dirs: 0, files: 1 }, "Alpha")).toBe("Deleted 1 file from Alpha");
    expect(deleteDoneText({ dirs: 2, files: 0 }, "Alpha")).toBe("Deleted 2 folders from Alpha");
  });

  it("names the selection and its place", () => {
    expect(deleteSelectionLine(["M42", "a", "b"], "Alpha", "EMMC Images", "Preview")).toBe(
      "'M42' and 2 other items in Alpha › EMMC Images › Preview",
    );
    expect(deleteSelectionLine(["M42"], "Alpha", "EMMC Images", "")).toBe(
      "'M42' in Alpha › EMMC Images",
    );
  });
});

/* ---- reasons -------------------------------------------------------------- */

const ctx: ReasonContext = { op: "copy", rig: "Bravo", sourceRig: "Alpha", name: "ref.fit" };

describe("reasonText", () => {
  const cases: [FilesReasonCode, Partial<ReasonContext>, string | RegExp, "overwrite" | "retry" | null][] = [
    ["chose_skip", {}, "Already exists: you chose Skip.", "overwrite"],
    ["appeared", {}, "Appeared on Bravo after the check, so it was not overwritten.", "overwrite"],
    ["type_conflict", {}, "A file and a folder cannot replace each other.", null],
    ["source_gone", {}, "No longer on Alpha.", null],
    ["changed", {}, "Changed on Bravo since you confirmed, so it was not overwritten.", "overwrite"],
    ["changed", { op: "delete" }, "Changed on Bravo since you confirmed, so it was not deleted.", null],
    ["not_empty", {}, "Folder not removed: Bravo reports it still contains files.", null],
    ["kept_marker", {}, "Kept: it holds Commander's device-identity file.", null],
    ["denied", {}, "Bravo refused: access denied.", "retry"],
    ["unreachable", {}, /^Bravo is unreachable \(tried 4 times\)/, "retry"],
    ["identity", { address: "10.0.0.5" }, "The device at 10.0.0.5 is not Bravo. Commander will not touch it.", null],
    ["not_attempted", { cause: "unreachable" }, "Not attempted: Bravo became unreachable.", "retry"],
    ["not_attempted", { cause: "stopped" }, "Not attempted: the operation was stopped.", "retry"],
    ["not_attempted", { cause: "lost" }, "Not attempted: the connection to Commander was lost.", "retry"],
    [
      "not_attempted",
      { cause: "identity" },
      "Not attempted: Commander could not confirm it was talking to Bravo.",
      "retry",
    ],
    ["error", { message: "disk full" }, "disk full", "retry"],
  ];

  it.each(cases)("words %s (%j)", (code, over, text, action) => {
    const r = reasonText(code, { ...ctx, ...over });
    if (typeof text === "string") expect(r.text).toBe(text);
    else expect(r.text).toMatch(text);
    expect(r.action).toBe(action);
  });

  it("words a copied file whose timestamp could not be set, with no action", () => {
    expect(reasonText("mtime_not_preserved", ctx)).toEqual({
      text: "Copied; timestamp not preserved, so it shows the copy time.",
      action: null,
    });
  });

  it("words a sharing violation as in use, with Retry", () => {
    expect(reasonText("in_use", ctx)).toEqual({
      text: "ref.fit is in use on Bravo (the ASIAIR or Commander may be reading or writing it).",
      action: "retry",
    });
  });

  it("words a stopped item per operation", () => {
    expect(reasonText("stopped", ctx).text).toBe(
      "Stopped while copying. The destination holds either the complete file or nothing under its name; Retry copies it again.",
    );
    expect(reasonText("stopped", { ...ctx, op: "delete" }).text).toBe(
      "Stopped while deleting; it may already be gone. The pane shows its current state.",
    );
  });

  it("words an upload's notes and its stopped row", () => {
    const up: ReasonContext = { op: "upload", rig: "Bravo", name: "ref.fit" };
    expect(reasonText("mtime_not_preserved", up)).toEqual({
      text: "Uploaded; timestamp not preserved, so it shows the upload time.",
      action: null,
    });
    const stopped =
      "Stopped while uploading. Bravo holds either the complete file or nothing under its name; Retry writes it again from Commander; if it had finished, Retry reports that it appeared on Bravo (or changed, for a file you chose to overwrite); you can leave it.";
    expect(reasonText("stopped", up)).toEqual({ text: stopped, action: "retry" });
    expect(reasonText("stopped", { ...up, overwriting: true }).text).toBe(
      `${stopped} The old file may already have been removed.`,
    );
    // appeared and changed offer Overwrite, as for a copy.
    expect(reasonText("appeared", up).action).toBe("overwrite");
    expect(reasonText("changed", up).action).toBe("overwrite");
  });

  it("words a reserved path met at write time with the server's sentence and no action", () => {
    const msg = "Autorun would stand where EMMC Images/Autorun must go. Leave Autorun out of the selection, or rename it on Alpha first.";
    expect(reasonText("watched", { ...ctx, message: msg })).toEqual({ text: msg, action: null });
  });

  it("picks the rig a cross-rig message names", () => {
    expect(rigNamedIn("Alpha: connection reset", ["Alpha", "Bravo"], "Bravo")).toBe("Alpha");
    expect(rigNamedIn("connection reset", ["Alpha", "Bravo"], "Bravo")).toBe("Bravo");
    expect(rigNamedIn(undefined, ["Alpha"], "Bravo")).toBe("Bravo");
  });
});

describe("abortedSuffix", () => {
  it("names the rig and why the server ended the run", () => {
    expect(abortedSuffix("", "Bravo")).toBe("");
    expect(abortedSuffix("unreachable", "Bravo")).toBe(" — Bravo became unreachable");
    expect(abortedSuffix("identity", "Bravo")).toBe(" — Commander could not confirm it was talking to Bravo");
  });
});

describe("overwriteNeedsConfirm", () => {
  it.each([
    ["chose_skip", "unarchived", true],
    ["chose_skip", "archived", false],
    ["chose_skip", "", false],
    ["chose_skip", undefined, false],
    ["appeared", undefined, true],
    ["changed", undefined, true],
    ["appeared", "", true],
    ["changed", "", true],
  ] as const)("%s with ledger %j → %s", (code, ledger, want) => {
    expect(overwriteNeedsConfirm({ code, dst_ledger: ledger })).toBe(want);
  });

  it("never asks for a reason that offers no Overwrite", () => {
    const others: FilesReasonCode[] = [
      "type_conflict",
      "source_gone",
      "already_gone",
      "not_empty",
      "kept_marker",
      "denied",
      "in_use",
      "unreachable",
      "identity",
      "mtime_not_preserved",
      "stopped",
      "not_attempted",
      "watched",
      "error",
    ];
    for (const code of others) expect(overwriteNeedsConfirm({ code })).toBe(false);
  });
});

describe("overwriteConfirmText", () => {
  const base = { name: "ref.fit", rig: "Bravo" };

  it("warns that an unarchived destination has no archived copy", () => {
    expect(
      overwriteConfirmText({ ...base, code: "chose_skip", dst_ledger: "unarchived" }, true),
    ).toBe("Overwrite ref.fit on Bravo? It has no archived copy.");
  });

  it("says the archive status is unknown for a change in a watched folder", () => {
    for (const code of ["appeared", "changed"] as const) {
      expect(overwriteConfirmText({ ...base, code }, true)).toBe(
        "Overwrite ref.fit on Bravo? It changed after the check, and Commander has not checked whether it has an archived copy.",
      );
    }
  });

  it("never mentions archiving for a folder Commander does not watch", () => {
    for (const code of ["appeared", "changed"] as const) {
      const text = overwriteConfirmText({ ...base, code }, false);
      expect(text).toBe("Overwrite ref.fit on Bravo? It changed after the check.");
      expect(text).not.toMatch(/archiv/i);
    }
  });
});

/* ---- the run report ------------------------------------------------------- */

const line = (rel: string, over: Partial<FilesItemLine> = {}): FilesItemLine => ({
  type: "item",
  rel,
  outcome: "skipped",
  code: "appeared",
  ...over,
});

describe("mergeRunReport", () => {
  const order = ["a", "b", "c", "d", "e"];

  it("counts items with no line as done once the summary arrived", () => {
    const { rows, end } = mergeRunReport({
      order,
      lines: [line("b"), line("d", { outcome: "copied", code: "mtime_not_preserved" })],
      lastCurrent: "e",
      summary: { type: "summary", done: 4, skipped: 1, failed: 0, aborted: "" },
      stoppedByUser: false,
    });
    expect(end).toBe("summary");
    expect(rows.map((r) => [r.rel, r.outcome, r.code])).toEqual([
      ["a", "done", null],
      ["b", "skipped", "appeared"],
      ["c", "done", null],
      ["d", "done", "mtime_not_preserved"],
      ["e", "done", null],
    ]);
    expect(rows.filter(isListed).map((r) => r.rel)).toEqual(["b", "d"]);
    expect(reportCounts(rows)).toEqual({ done: 4, skipped: 1, failed: 0, notAttempted: 0 });
  });

  it("builds a stopped report: earlier unreported done, in-flight stopped, later not attempted", () => {
    const { rows, end } = mergeRunReport({
      order,
      lines: [line("b", { outcome: "failed", code: "denied" })],
      lastCurrent: "c",
      summary: null,
      stoppedByUser: true,
    });
    expect(end).toBe("stopped");
    expect(rows.map((r) => [r.rel, r.outcome, r.code, r.cause])).toEqual([
      ["a", "done", null, undefined],
      ["b", "failed", "denied", undefined],
      ["c", "failed", "stopped", undefined],
      ["d", "failed", "not_attempted", "stopped"],
      ["e", "failed", "not_attempted", "stopped"],
    ]);
  });

  it("reads a missing summary without Stop as a lost connection", () => {
    const { rows, end } = mergeRunReport({
      order,
      lines: [],
      lastCurrent: "d",
      summary: null,
      stoppedByUser: false,
    });
    expect(end).toBe("interrupted");
    expect(rows[4]).toMatchObject({ code: "not_attempted", cause: "lost" });
  });

  it("keeps the in-flight item's own line when it finished before the cut", () => {
    const { rows } = mergeRunReport({
      order,
      lines: [line("c")],
      lastCurrent: "c",
      summary: null,
      stoppedByUser: true,
    });
    expect(rows[2]).toMatchObject({ outcome: "skipped", code: "appeared" });
  });

  it("marks everything not attempted when no item had started", () => {
    const { rows } = mergeRunReport({
      order: ["a"],
      lines: [],
      lastCurrent: null,
      summary: null,
      stoppedByUser: true,
    });
    expect(rows).toEqual([{ rel: "a", outcome: "failed", code: "not_attempted", cause: "stopped" }]);
  });

  it("does not list already-gone deletes, and reads server not_attempted as unreachable", () => {
    const { rows } = mergeRunReport({
      order: ["a", "b"],
      lines: [
        line("a", { outcome: "deleted", code: "already_gone" }),
        line("b", { outcome: "failed", code: "not_attempted" }),
      ],
      lastCurrent: "a",
      summary: { type: "summary", done: 1, skipped: 0, failed: 1, aborted: "unreachable" },
      stoppedByUser: false,
    });
    expect(rows[0]).toMatchObject({ outcome: "done", code: null });
    expect(isListed(rows[0]!)).toBe(false);
    expect(rows[1]).toMatchObject({ code: "not_attempted", cause: "unreachable" });
  });

  it("reads not_attempted after an identity abort as identity, not unreachable", () => {
    const { rows } = mergeRunReport({
      order: ["a", "b"],
      lines: [
        line("a", { outcome: "failed", code: "identity" }),
        line("b", { outcome: "failed", code: "not_attempted" }),
      ],
      lastCurrent: "a",
      summary: { type: "summary", done: 0, skipped: 0, failed: 2, aborted: "identity" },
      stoppedByUser: false,
    });
    expect(rows[1]).toMatchObject({ code: "not_attempted", cause: "identity" });
  });

  it("lists a folder the run could not create, ahead of the items", () => {
    const { rows } = mergeRunReport({
      order: ["Empty/x.fit"],
      lines: [line("Empty", { outcome: "failed", code: "denied", message: "denied" })],
      lastCurrent: null,
      summary: { type: "summary", done: 1, skipped: 0, failed: 1, aborted: "" },
      stoppedByUser: false,
    });
    expect(rows.map((r) => [r.rel, r.outcome, r.code, r.dir])).toEqual([
      ["Empty", "failed", "denied", true],
      ["Empty/x.fit", "done", null, undefined],
    ]);
    expect(rows.filter(isListed).map((r) => r.rel)).toEqual(["Empty"]);
  });

  it("updates rows in place after a row action, keeping a chose_skip row's ledger", () => {
    const rows = [
      { rel: "a", outcome: "skipped" as const, code: "chose_skip" as const, dst_ledger: "unarchived" as const },
      { rel: "b", outcome: "failed" as const, code: "denied" as const },
    ];
    const next = applyRowUpdates(rows, [{ rel: "b", outcome: "done", code: null }]);
    expect(next[0]).toBe(rows[0]);
    expect(next[1]).toMatchObject({ rel: "b", outcome: "done", code: null });
  });
});

describe("report counts", () => {
  it("keeps stopped and not-attempted rows out of failed, and agrees with the server's summary", () => {
    const summary = { type: "summary" as const, done: 1, skipped: 1, failed: 3, aborted: "unreachable" as const };
    const { rows } = mergeRunReport({
      order: ["a", "b", "c", "d", "e"],
      lines: [
        line("b"),
        line("c", { outcome: "failed", code: "unreachable" }),
        line("d", { outcome: "failed", code: "not_attempted" }),
        line("e", { outcome: "failed", code: "not_attempted" }),
      ],
      lastCurrent: "c",
      summary,
      stoppedByUser: false,
    });
    const c = reportCounts(rows);
    expect(c).toEqual({ done: 1, skipped: 1, failed: 1, notAttempted: 2 });
    expect(c.done).toBe(summary.done);
    expect(c.skipped).toBe(summary.skipped);
    expect(c.failed + c.notAttempted).toBe(summary.failed);
  });

  it("counts a stopped row as not attempted", () => {
    const rows = [
      { rel: "a", outcome: "failed" as const, code: "stopped" as const },
      { rel: "b", outcome: "failed" as const, code: "not_attempted" as const, cause: "stopped" as const },
      { rel: "c", outcome: "failed" as const, code: "denied" as const },
    ];
    expect(rows.map(isNotAttempted)).toEqual([true, true, false]);
    expect(reportCounts(rows)).toEqual({ done: 0, skipped: 0, failed: 1, notAttempted: 2 });
  });
});

describe("row actions", () => {
  it("labels each outcome", () => {
    expect(rowOutcomeLabel({ rel: "a", outcome: "done", code: null }, "copy")).toBe("Copied");
    expect(rowOutcomeLabel({ rel: "a", outcome: "done", code: null }, "delete")).toBe("Deleted");
    expect(rowOutcomeLabel({ rel: "a", outcome: "skipped", code: "appeared" }, "copy")).toBe("Skipped");
    expect(rowOutcomeLabel({ rel: "a", outcome: "failed", code: "stopped" }, "copy")).toBe("Stopped");
    expect(rowOutcomeLabel({ rel: "a", outcome: "failed", code: "not_attempted" }, "copy")).toBe("Not attempted");
    expect(rowOutcomeLabel({ rel: "a", outcome: "failed", code: "denied" }, "copy")).toBe("Failed");
  });

  it("announces one row by name and several as counts", () => {
    expect(rowRunAnnouncement([{ rel: "M42/a.fit", outcome: "done", code: null }], "copy")).toBe("a.fit: Copied.");
    expect(
      rowRunAnnouncement(
        [
          { rel: "a", outcome: "done", code: null },
          { rel: "b", outcome: "done", code: null },
          { rel: "c", outcome: "failed", code: "denied" },
          { rel: "d", outcome: "failed", code: "not_attempted", cause: "stopped" },
        ],
        "delete",
      ),
    ).toBe("Retried 4: 2 deleted, 1 failed, 1 not attempted.");
  });

  it("settles rows as their lines arrive: lined items, and clean items the run moved past", () => {
    const order = ["a", "b", "c", "d"];
    expect(settledRows(order, [], null)).toEqual([]);
    expect(settledRows(order, [], "a")).toEqual([]);
    expect(settledRows(order, [line("b", { outcome: "failed", code: "denied" })], "c")).toEqual([
      { rel: "a", outcome: "done", code: null },
      { rel: "b", outcome: "failed", code: "denied", message: undefined, dst_size: undefined, dst_mtime: undefined, cause: undefined },
    ]);
  });
});

/* ---- waiting -------------------------------------------------------------- */

describe("waiting text", () => {
  it("waits 5 s", () => {
    expect(WAITING_AFTER_MS).toBe(5000);
  });

  it("names one rig, or both of a cross-rig copy", () => {
    expect(waitingText(["Alpha"])).toBe("Waiting for Alpha to respond…");
    expect(waitingText(["Alpha", "Bravo"])).toBe("Waiting for Alpha and Bravo to respond…");
    expect(waitingText(["Alpha", "Alpha"])).toBe("Waiting for Alpha to respond…");
  });
});

/* ---- upload and rename wording ------------------------------------------ */

describe("upload wording", () => {
  it("labels an upload's run like a copy's and a delete's", () => {
    expect(PROGRESS_LABEL).toEqual({
      copy: "Copy progress",
      delete: "Delete progress",
      upload: "Upload progress",
    });
    expect(RUNNING_ROW_LABEL.upload).toBe("Uploading…");
    expect(rowOutcomeLabel({ rel: "ref.fit", outcome: "done", code: null }, "upload")).toBe("Uploaded");
    expect(rowRunAnnouncement([{ rel: "ref.fit", outcome: "done", code: null }], "upload")).toBe(
      "ref.fit: Uploaded.",
    );
    expect(
      rowRunAnnouncement(
        [
          { rel: "a", outcome: "done", code: null },
          { rel: "b", outcome: "failed", code: "denied" },
        ],
        "upload",
      ),
    ).toBe("Retried 2: 1 uploaded, 1 failed.");
  });

  it("asks before leaving a running upload", () => {
    expect(leaveRunningText("upload")).toBe(
      "An upload is running. Leaving this page stops it; the report of what was done would be lost.",
    );
    expect(leaveRunningText("copy")).toMatch(/^A copy is running on your ASIAIR\./);
  });

  it("words the Sending state", () => {
    expect(sendingText("ref.fit")).toBe("Sending ref.fit to Commander");
    expect(sendingNote("Bravo")).toBe("Nothing is written to Bravo until you confirm the upload.");
    expect(sendFailedText("ref.fit", "Bravo")).toBe(
      "Couldn't send ref.fit to Commander: the connection was lost, or your sign-in ended. Nothing was written to Bravo.",
    );
    expect(nonJsonUploadRefusal(413)).toBe(
      "Commander, or a proxy in front of it, refused the upload (HTTP 413).",
    );
  });

  it("words checking, the plan and the result", () => {
    expect(placeText("Bravo", "EMMC Images", "")).toBe("Bravo › EMMC Images");
    expect(placeText("Bravo", "EMMC Images", "Plan/M42")).toBe("Bravo › EMMC Images › Plan › M42");
    expect(uploadCheckingText("ref.fit", "Bravo", "EMMC Images", "Preview")).toBe(
      "Checking ref.fit against Bravo › EMMC Images › Preview…",
    );
    expect(uploadSkipReason("Bravo")).toBe(
      "Nothing to upload: you chose Skip. Choose Overwrite to replace the file on Bravo.",
    );
    expect(sendStoppedText("Bravo")).toBe("Upload stopped. Nothing was written to Bravo.");
    expect(uploadPlanTitle("ref.fit", 412 * 1024 * 1024, "Bravo")).toBe("Upload ref.fit (412.0 MB) to Bravo");
    expect(uploadTypeConflictText("ref.fit")).toBe("ref.fit can't be uploaded here: a folder has that name.");
    expect(uploadWritingText("ref.fit", "Bravo")).toBe("Writing ref.fit to Bravo");
    expect(uploadDoneText("ref.fit", 1024, "Bravo")).toBe("Uploaded ref.fit (1.0 KB) to Bravo");
  });

  it("warns about space only when the file is larger than the share's free space", () => {
    const gb = 1024 ** 3;
    expect(uploadSpaceWarning(12 * gb, 9 * gb, "EMMC Images", "Bravo")).toBe(
      "This file needs 12.0 GB; EMMC Images on Bravo has 9.0 GB free.",
    );
    expect(uploadSpaceWarning(9 * gb, 9 * gb, "EMMC Images", "Bravo")).toBeNull();
    expect(uploadSpaceWarning(12 * gb, undefined, "EMMC Images", "Bravo")).toBeNull();
  });

  it("titles the result by how the run ended", () => {
    expect(uploadReportTitle("stopped", "failed", "ref.fit", "Bravo")).toBe("Upload stopped");
    expect(uploadReportTitle("interrupted", "failed", "ref.fit", "Bravo")).toBe(
      "Upload interrupted — lost connection to Commander, or your sign-in ended",
    );
    expect(uploadReportTitle("summary", "done", "ref.fit", "Bravo")).toBe("Uploaded ref.fit to Bravo");
    expect(uploadReportTitle("summary", "skipped", "ref.fit", "Bravo")).toBe("ref.fit was not uploaded to Bravo");
    expect(uploadReportTitle("summary", "failed", "ref.fit", "Bravo")).toBe("Couldn't upload ref.fit to Bravo");
  });

  it("leads Plan failed with a reserved-path refusal's own sentence", () => {
    const reserved = "Autorun would stand where EMMC Images/Autorun must go. Leave Autorun out of the selection, or rename it on Alpha first.";
    expect(planFailedText("copy", "watched", reserved, "Alpha", "Bravo")).toBe(reserved);
    expect(planFailedText("upload", "watched", "x", "", "Bravo")).toBe("x");
    expect(planFailedText("copy", "busy", "Another operation is running.", "Alpha", "Bravo")).toBe(
      "Couldn't check the copy from Alpha to Bravo: Another operation is running.",
    );
    expect(planFailedText("upload", "busy", "Another operation is running.", "", "Bravo")).toBe(
      "Couldn't check the upload to Bravo: Another operation is running.",
    );
  });
});

describe("rename wording", () => {
  const c = { rigName: "Bravo", name: "ref.fit", newName: "REF.FIT" };

  it("words exists once, whatever the collision", () => {
    expect(renameErrorText("exists", "server text", c)).toBe(
      "Bravo already has an item named REF.FIT here (names that differ only in letter case count as the same). Nothing was replaced.",
    );
  });

  it("words in_use and not_found, and keeps the server's sentence for the rest", () => {
    expect(renameErrorText("in_use", "x", c)).toBe(
      "ref.fit is in use on Bravo (the ASIAIR may be writing it). Try again in a moment.",
    );
    expect(renameErrorText("not_found", "x", c)).toBe(
      "ref.fit isn't in this folder any more. If the connection dropped, the rename may already have gone through; the list now shows the folder as it is.",
    );
    for (const code of ["watched", "layout", "busy", "unreachable", "identity", "host_bound", "denied"]) {
      expect(renameErrorText(code, `the ${code} sentence`, c)).toBe(`the ${code} sentence`);
    }
  });

  it("confirms a rename by name", () => {
    expect(renameDoneText("ref.fit", "dark.fit")).toBe("Renamed ref.fit to dark.fit");
  });

  it("says once, in a watched folder's notice, that its items can't be renamed", () => {
    expect(WATCHED_RENAME_NOTE).toBe("Items here can't be renamed.");
    expect(watchedFolderNotice("Bravo", false)).toMatch(/after the grace period\. Items here can't be renamed\.$/);
    expect(watchedFolderNotice("Bravo", true)).toMatch(/left on Bravo\. Items here can't be renamed\.$/);
  });
});
