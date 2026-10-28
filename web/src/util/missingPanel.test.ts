import { describe, expect, it } from "vitest";

import {
  ackAllCount,
  emptyActionableText,
  forgetButtonLabel,
  forgetDoneText,
  forgetScopeText,
  initialSectionFilter,
  pageCount,
  reconcileFilter,
  skippedSummary,
  withDir,
  withQuery,
} from "./missingPanel";

const onPage3 = { ...initialSectionFilter, page: 3 };

describe("section filter", () => {
  it("returns to page 1 when the folder changes", () => {
    expect(withDir(onPage3, "/lib/M31/NIGHT_2")).toEqual({ dir: "/lib/M31/NIGHT_2", q: "", page: 1 });
    expect(withDir({ ...onPage3, dir: "/lib/a" }, "")).toEqual({ dir: "", q: "", page: 1 });
  });

  it("returns to page 1 when the search changes", () => {
    expect(withQuery(onPage3, "ha")).toEqual({ dir: "", q: "ha", page: 1 });
  });

  it("keeps the page, and the same object, when nothing changes", () => {
    const f = { dir: "/lib/a", q: "ha", page: 3 };
    expect(withDir(f, "/lib/a")).toBe(f);
    expect(withQuery(f, "ha")).toBe(f);
  });

  it("adopts the page the server answered when the one on screen emptied", () => {
    expect(reconcileFilter(onPage3, { page: 2, groups: [] })).toEqual({ ...onPage3, page: 2 });
    expect(reconcileFilter(onPage3, { page: 3, groups: [] })).toBe(onPage3);
  });

  it("clears a folder filter whose folder left the section", () => {
    const f = { dir: "/lib/a", q: "ha", page: 2 };
    const a = { dir: "/lib/a", count: 1, ackable: 0 };
    const b = { dir: "/lib/b", count: 1, ackable: 0 };
    expect(reconcileFilter(f, { page: 2, groups: [b] })).toEqual({ dir: "", q: "ha", page: 1 });
    expect(reconcileFilter(f, { page: 2, groups: [a, b] })).toBe(f);
    // Byte-exact, as the server matches it: a case variant is another folder.
    expect(reconcileFilter(f, { page: 2, groups: [{ ...a, dir: "/lib/A" }] }).dir).toBe("");
  });

  it("counts pages, one at least", () => {
    expect(pageCount(0, 50)).toBe(1);
    expect(pageCount(50, 50)).toBe(1);
    expect(pageCount(51, 50)).toBe(2);
    expect(pageCount(10, 0)).toBe(1);
  });
});

describe("ackAllCount", () => {
  const g = { dir: "/lib/a", count: 3, ackable: 2 };

  it("names the acknowledgeable rows, not the folder's count", () => {
    expect(ackAllCount(g, false)).toBe(2);
  });

  it("hides the button when nothing is acknowledgeable", () => {
    expect(ackAllCount({ ...g, ackable: 0 }, false)).toBeNull();
  });

  it("hides the button on a keep_originals rig", () => {
    expect(ackAllCount(g, true)).toBeNull();
  });
});

describe("skippedSummary", () => {
  it("names leftovers apart from frames not verified", () => {
    expect(skippedSummary({ gone: 69, restore_pending: 1, not_verified: 2, already: 0 })).toBe(
      "69 already removed from the device, 1 with a pending restore, 2 not verified",
    );
  });

  it("is null when nothing was skipped", () => {
    expect(skippedSummary({ gone: 0, restore_pending: 0, not_verified: 0, already: 0 })).toBeNull();
  });
});

describe("emptyActionableText", () => {
  it("points at the leftovers while the missing warning is up", () => {
    expect(emptyActionableText(true)).toMatch(/under Already gone from the device/);
  });

  it("says plainly that nothing needs action otherwise", () => {
    expect(emptyActionableText(false)).toBe("No missing frames need action.");
  });
});

describe("forget copy", () => {
  it("labels the section and folder buttons", () => {
    expect(forgetButtonLabel({}, 60)).toBe("Forget all 60");
    expect(forgetButtonLabel({ older_than_days: 7 }, 60)).toBe("Forget older than 7 days");
    expect(forgetButtonLabel({ dir: "/lib/a" }, 4)).toBe("Forget all 4");
  });

  it("describes the scope the token was minted for", () => {
    expect(forgetScopeText({})).toBe("Every frame already gone from the device.");
    expect(forgetScopeText({ older_than_days: 28 })).toBe(
      "Every frame already gone from the device, missing for more than 28 days.",
    );
    expect(forgetScopeText({ dir: "/lib/a" })).toBe("Every frame in this folder.");
  });

  it("reports the server's count, or that the rig was busy", () => {
    expect(forgetDoneText({ forgotten: 1 })).toBe("Forgot 1 frame.");
    expect(forgetDoneText({ forgotten: 12 })).toBe("Forgot 12 frames.");
    expect(forgetDoneText({ code: "FORGET_QUEUED" })).toMatch(/busy/);
  });
});
