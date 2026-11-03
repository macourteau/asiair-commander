import { describe, expect, it } from "vitest";
import { EDIT_FIELDS, EDITABLE_KEYS, filterSuggestions, validateEditValue } from "./headerEdit";

describe("validateEditValue — OBJECT", () => {
  it("accepts a trimmed target and asks for one when blank", () => {
    expect(validateEditValue("OBJECT", "  NGC7000 ")).toBeUndefined();
    expect(validateEditValue("OBJECT", "")).toBe("Enter the corrected target name.");
    expect(validateEditValue("OBJECT", "   ")).toBe("Enter the corrected target name.");
  });

  it("allows characters the server sanitises rather than refusing them", () => {
    // OBJECT is sanitised and folded server-side; the client must not pre-empt that.
    expect(validateEditValue("OBJECT", "Sh2-101 / Tulip")).toBeUndefined();
    expect(validateEditValue("OBJECT", "Cœur")).toBeUndefined();
  });

  it("caps the trimmed length at 60 characters, counted as code points", () => {
    expect(validateEditValue("OBJECT", "x".repeat(60))).toBeUndefined();
    expect(validateEditValue("OBJECT", ` ${"x".repeat(60)} `)).toBeUndefined();
    expect(validateEditValue("OBJECT", "x".repeat(61))).toBe(
      "Target names must be 60 characters or fewer (currently 61).",
    );
    // 60 astral characters are 120 UTF-16 units but 60 runes, as the server counts.
    expect(validateEditValue("OBJECT", "\u{1F30C}".repeat(60))).toBeUndefined();
  });
});

describe("validateEditValue — FILTER", () => {
  it("accepts any printable ASCII, trimmed", () => {
    expect(validateEditValue("FILTER", " Ha ")).toBeUndefined();
    expect(validateEditValue("FILTER", "L-eXtreme 7nm (OIII+Ha)")).toBeUndefined();
    expect(validateEditValue("FILTER", " ~")).toBeUndefined();
  });

  it("asks for a value when blank", () => {
    expect(validateEditValue("FILTER", "")).toBe("Enter the corrected filter name.");
    expect(validateEditValue("FILTER", "\t ")).toBe("Enter the corrected filter name.");
  });

  it("names the character outside the FITS string set", () => {
    const accent = validateEditValue("FILTER", "Hα");
    expect(accent).toContain('"α"');
    expect(accent).toContain("plain ASCII");
    expect(validateEditValue("FILTER", "Ha\tOIII")).toContain("the control character U+0009");
    expect(validateEditValue("FILTER", "Ha\u007f")).toContain("U+007F");
    expect(validateEditValue("FILTER", "Ha\u{1F534}")).toContain('"\u{1F534}"');
  });

  it("caps the trimmed length at 60 characters", () => {
    expect(validateEditValue("FILTER", "F".repeat(60))).toBeUndefined();
    expect(validateEditValue("FILTER", "F".repeat(61))).toBe(
      "Filter names must be 60 characters or fewer (currently 61).",
    );
  });
});

describe("EDIT_FIELDS", () => {
  it("labels each key with its FITS keyword", () => {
    expect(EDITABLE_KEYS).toEqual(["OBJECT", "FILTER"]);
    for (const key of EDITABLE_KEYS) {
      expect(EDIT_FIELDS[key].option).toContain(`(${key})`);
      expect(EDIT_FIELDS[key].help).toContain(key);
    }
  });
});

describe("filterSuggestions", () => {
  it("lists the distinct non-empty filters, sorted", () => {
    expect(
      filterSuggestions([
        { filter: "OIII" },
        { filter: "Ha" },
        {},
        { filter: "" },
        { filter: "  " },
        { filter: "Ha" },
        { filter: "L" },
      ]),
    ).toEqual(["Ha", "L", "OIII"]);
  });

  it("keeps spellings that differ only in case as separate suggestions", () => {
    const got = filterSuggestions([{ filter: "Ha" }, { filter: "HA" }]);
    expect(got).toHaveLength(2);
    expect(got).toContain("Ha");
    expect(got).toContain("HA");
  });

  it("is empty for an empty list", () => {
    expect(filterSuggestions([])).toEqual([]);
  });
});
