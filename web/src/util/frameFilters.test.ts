import { describe, expect, it } from "vitest";
import {
  availableFields,
  chipLabel,
  filterProblem,
  isActive,
  newFilter,
  NO_FILTER,
  toParams,
  type FrameFilter,
} from "./frameFilters";

const localMidnight = (d: string) => Math.floor(Date.parse(`${d}T00:00:00`) / 1000);

describe("availableFields", () => {
  it("offers each field once, dropping those in use", () => {
    expect(availableFields([])).toEqual([
      "device",
      "target",
      "filter",
      "type",
      "date",
      "exposure",
      "path",
    ]);
    expect(availableFields([newFilter("target"), newFilter("exposure")])).toEqual([
      "device",
      "filter",
      "type",
      "date",
      "path",
    ]);
  });
});

describe("toParams", () => {
  it("sends nothing for incomplete filters", () => {
    expect(toParams((["device", "target", "filter", "type", "date", "exposure", "path"] as const).map(newFilter))).toEqual({});
  });

  it("maps every complete filter onto its parameters", () => {
    const filters: FrameFilter[] = [
      { field: "device", not: true, values: ["rig-a", "rig-b"] },
      { field: "target", text: "  NGC 1499 " },
      { field: "path", text: "Flats" },
      { field: "filter", not: false, values: ["Ha", NO_FILTER] },
      { field: "type", not: true, values: ["FLAT"] },
      { field: "date", not: true, from: "2026-11-01", to: "2026-11-03" },
      { field: "exposure", op: "in", min: "60", max: "300.5" },
    ];
    expect(toParams(filters)).toEqual({
      rig: ["rig-a", "rig-b"],
      rig_not: 1,
      target: "NGC 1499",
      path: "Flats",
      filter: ["Ha"],
      filter_none: 1,
      type: ["FLAT"],
      type_not: 1,
      obs_from: localMidnight("2026-11-01"),
      obs_to: localMidnight("2026-11-03"),
      obs_not: 1,
      exp_op: "in",
      exp_min: 60,
      exp_max: 300.5,
    });
  });

  it("sends only <none> as filter_none, and a one-sided date as one bound", () => {
    expect(toParams([{ field: "filter", not: true, values: [NO_FILTER] }])).toEqual({
      filter_none: 1,
      filter_not: 1,
    });
    expect(toParams([{ field: "date", not: false, from: "", to: "2026-11-03" }])).toEqual({
      obs_from: undefined,
      obs_to: localMidnight("2026-11-03"),
    });
  });

  it("reads one bound for a single-bound exposure op and ignores a stale max", () => {
    expect(toParams([{ field: "exposure", op: "le", min: "1.325", max: "9" }])).toEqual({
      exp_op: "le",
      exp_min: 1.325,
    });
  });

  it("holds back an invalid filter rather than sending it", () => {
    const bad: FrameFilter[] = [
      { field: "date", not: false, from: "2026-11-03", to: "2026-11-03" },
      { field: "exposure", op: "in", min: "300", max: "60" },
      { field: "exposure", op: "lt", min: "abc", max: "" },
      { field: "exposure", op: "gt", min: "-1", max: "" },
    ];
    for (const f of bad) {
      expect(filterProblem(f)).toBeTruthy();
      expect(isActive(f)).toBe(false);
      expect(toParams([f])).toEqual({});
    }
  });
});

describe("chipLabel", () => {
  const names: Record<string, string> = { "rig-a": "iEXOS", LIGHT: "Light" };
  const label = (f: FrameFilter) => chipLabel(f, (v) => names[v] ?? v);

  it("reads as a sentence with names in place of ids", () => {
    expect(label({ field: "device", not: false, values: ["rig-a"] })).toBe("Device is iEXOS");
    expect(label({ field: "type", not: true, values: ["LIGHT"] })).toBe("Frame type is not Light");
    expect(label({ field: "filter", not: false, values: ["L", NO_FILTER] })).toBe("Filter is L, <none>");
    expect(label({ field: "target", text: "NGC" })).toBe("Target contains “NGC”");
    expect(label({ field: "exposure", op: "le", min: "60", max: "" })).toBe("Exposure ≤ 60s");
    expect(label({ field: "exposure", op: "out", min: "60", max: "300" })).toBe("Exposure not in 60–300s");
  });

  it("names three values and counts the rest", () => {
    expect(label({ field: "filter", not: false, values: ["L", "R", "G", "B", "Ha"] })).toBe(
      "Filter is L, R, G +2",
    );
  });

  it("states a negated one-sided date as the other side", () => {
    expect(label({ field: "date", not: false, from: "2026-11-01", to: "" })).toBe("Date from 2026-11-01");
    expect(label({ field: "date", not: true, from: "2026-11-01", to: "" })).toBe("Date before 2026-11-01");
    expect(label({ field: "date", not: true, from: "", to: "2026-11-03" })).toBe("Date from 2026-11-03");
    expect(label({ field: "date", not: true, from: "2026-11-01", to: "2026-11-03" })).toBe(
      "Date not 2026-11-01 → 2026-11-03",
    );
  });

  it("marks an incomplete filter with an ellipsis", () => {
    expect(label(newFilter("path"))).toBe("Path contains…");
    expect(label(newFilter("device"))).toBe("Device is…");
    expect(label(newFilter("exposure"))).toBe("Exposure ≥…");
  });
});
