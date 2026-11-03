/**
 * The Find frames filter bar's model (design header-edits §9.4): which filters
 * exist, how each one reads as a chip, when it is complete enough to apply, and
 * how the set maps onto GET /api/frames's parameters. The server does the
 * matching across the whole transfer history, not just the loaded page.
 *
 * Each field can be used once. A "not" filter is the exact complement of its
 * positive form, so a frame missing the field (no FILTER card, no DATE-OBS, no
 * recorded exposure) matches "is not" / "not in" and never "is" / "in".
 */
import type { HistoryFilterParams } from "@/api";

export type FilterField = "device" | "target" | "filter" | "type" | "date" | "exposure" | "path";

export type ExposureOp = NonNullable<HistoryFilterParams["exp_op"]>;

export type FrameFilter =
  | { field: "target" | "path"; text: string }
  /** values: rig ids (device), FILTER values with "" for no FILTER card
   * (filter), or canonical frame types (type). */
  | { field: "device" | "filter" | "type"; not: boolean; values: string[] }
  /** from/to: YYYY-MM-DD in local time; from is included, to is excluded. */
  | { field: "date"; not: boolean; from: string; to: string }
  /** min/max: seconds as typed; the single-bound ops read min. */
  | { field: "exposure"; op: ExposureOp; min: string; max: string };

/** The value the filter picker uses for "no FILTER card". */
export const NO_FILTER = "";
export const NO_FILTER_LABEL = "<none>";

/** Every field, in the add menu's order, with its menu label. */
export const FILTER_FIELDS: { field: FilterField; label: string }[] = [
  { field: "device", label: "Device" },
  { field: "target", label: "Target" },
  { field: "filter", label: "Filter" },
  { field: "type", label: "Frame type" },
  { field: "date", label: "Date observed" },
  { field: "exposure", label: "Exposure" },
  { field: "path", label: "Path" },
];

export const EXPOSURE_OPS: { op: ExposureOp; label: string; symbol: string }[] = [
  { op: "lt", label: "less than", symbol: "<" },
  { op: "le", label: "at most", symbol: "≤" },
  { op: "gt", label: "more than", symbol: ">" },
  { op: "ge", label: "at least", symbol: "≥" },
  { op: "in", label: "in range", symbol: "in" },
  { op: "out", label: "not in range", symbol: "not in" },
];

export const isRangeOp = (op: ExposureOp) => op === "in" || op === "out";

export function fieldLabel(field: FilterField): string {
  return FILTER_FIELDS.find((f) => f.field === field)?.label ?? field;
}

export function newFilter(field: FilterField): FrameFilter {
  switch (field) {
    case "target":
    case "path":
      return { field, text: "" };
    case "device":
    case "filter":
    case "type":
      return { field, not: false, values: [] };
    case "date":
      return { field, not: false, from: "", to: "" };
    case "exposure":
      return { field, op: "ge", min: "", max: "" };
  }
}

/** The fields not yet in use, in menu order: each field can be used once. */
export function availableFields(filters: readonly FrameFilter[]): FilterField[] {
  const used = new Set(filters.map((f) => f.field));
  return FILTER_FIELDS.map((f) => f.field).filter((f) => !used.has(f));
}

function seconds(s: string): number | undefined {
  if (s.trim() === "") return undefined;
  const n = Number(s);
  return Number.isFinite(n) ? n : undefined;
}

/** A YYYY-MM-DD date input value → unix seconds at local midnight. */
function localDayStart(value: string): number | undefined {
  if (!value) return undefined;
  const t = Date.parse(`${value}T00:00:00`);
  return Number.isNaN(t) ? undefined : Math.floor(t / 1000);
}

/**
 * What is wrong with a filter as entered, for its editor to show; undefined
 * when nothing is (an empty filter is incomplete, not wrong).
 */
export function filterProblem(f: FrameFilter): string | undefined {
  if (f.field === "date") {
    const from = localDayStart(f.from);
    const to = localDayStart(f.to);
    if (from !== undefined && to !== undefined && to <= from) {
      return "The end date is excluded, so it must come after the start date.";
    }
  }
  if (f.field === "exposure") {
    const min = seconds(f.min);
    const max = seconds(f.max);
    if ((f.min.trim() !== "" && min === undefined) || (f.max.trim() !== "" && max === undefined)) {
      return "Enter the exposure as a number of seconds.";
    }
    if ((min ?? 0) < 0 || (max ?? 0) < 0) return "Exposure can't be negative.";
    if (isRangeOp(f.op) && min !== undefined && max !== undefined && max < min) {
      return "The range must run from the smaller to the larger value.";
    }
  }
  return undefined;
}

/** Whether a filter narrows anything yet: an incomplete or invalid one is shown
 * but not sent. */
export function isActive(f: FrameFilter): boolean {
  if (filterProblem(f)) return false;
  switch (f.field) {
    case "target":
    case "path":
      return f.text.trim() !== "";
    case "device":
    case "filter":
    case "type":
      return f.values.length > 0;
    case "date":
      return f.from !== "" || f.to !== "";
    case "exposure":
      return seconds(f.min) !== undefined && (!isRangeOp(f.op) || seconds(f.max) !== undefined);
  }
}

/** The active filters as GET /api/frames parameters. */
export function toParams(filters: readonly FrameFilter[]): HistoryFilterParams {
  const p: HistoryFilterParams = {};
  for (const f of filters) {
    if (!isActive(f)) continue;
    switch (f.field) {
      case "target":
        p.target = f.text.trim();
        break;
      case "path":
        p.path = f.text.trim();
        break;
      case "device":
        p.rig = f.values;
        if (f.not) p.rig_not = 1;
        break;
      case "filter": {
        const named = f.values.filter((v) => v !== NO_FILTER);
        if (named.length) p.filter = named;
        if (f.values.includes(NO_FILTER)) p.filter_none = 1;
        if (f.not) p.filter_not = 1;
        break;
      }
      case "type":
        p.type = f.values;
        if (f.not) p.type_not = 1;
        break;
      case "date":
        p.obs_from = localDayStart(f.from);
        p.obs_to = localDayStart(f.to);
        if (f.not) p.obs_not = 1;
        break;
      case "exposure":
        p.exp_op = f.op;
        p.exp_min = seconds(f.min);
        if (isRangeOp(f.op)) p.exp_max = seconds(f.max);
        break;
    }
  }
  return p;
}

/** At most three values are named on a chip; the rest are counted. */
function valueList(names: string[]): string {
  const shown = names.slice(0, 3).join(", ");
  return names.length > 3 ? `${shown} +${names.length - 3}` : shown;
}

/**
 * The chip's text. valueLabel turns a stored value (a rig id, a frame type)
 * into what the operator recognises (the device name, "Light").
 */
export function chipLabel(f: FrameFilter, valueLabel: (v: string) => string = (v) => v): string {
  const name = fieldLabel(f.field);
  switch (f.field) {
    case "target":
    case "path":
      return f.text.trim() ? `${name} contains “${f.text.trim()}”` : `${name} contains…`;
    case "device":
    case "filter":
    case "type": {
      if (!f.values.length) return `${name} ${f.not ? "is not" : "is"}…`;
      const names = f.values.map((v) =>
        f.field === "filter" && v === NO_FILTER ? NO_FILTER_LABEL : valueLabel(v),
      );
      return `${name} ${f.not ? "is not" : "is"} ${valueList(names)}`;
    }
    case "date": {
      // A one-sided range negated is the other side: not [from, ∞) is before from.
      if (f.from && f.to) return `Date ${f.not ? "not " : ""}${f.from} → ${f.to}`;
      if (f.from) return f.not ? `Date before ${f.from}` : `Date from ${f.from}`;
      if (f.to) return f.not ? `Date from ${f.to}` : `Date before ${f.to}`;
      return `Date ${f.not ? "not in" : "in"}…`;
    }
    case "exposure": {
      const op = EXPOSURE_OPS.find((o) => o.op === f.op)?.symbol ?? f.op;
      const min = seconds(f.min);
      const max = seconds(f.max);
      if (min === undefined || (isRangeOp(f.op) && max === undefined)) return `Exposure ${op}…`;
      return isRangeOp(f.op)
        ? `Exposure ${op} ${min}–${max}s`
        : `Exposure ${op} ${min}s`;
    }
  }
}
