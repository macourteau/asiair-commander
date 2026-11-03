/**
 * FilterBar — the Find frames list's chip filters (design header-edits §9.4).
 * Each filter is a chip that opens its editor in a popover and carries an ×
 * to remove it; the "Add filter" chip opens a menu of the fields not yet used.
 * A filter added from the menu opens its editor at once, and one closed while
 * still empty is dropped, so an abandoned add leaves nothing behind.
 *
 * The model (labels, validation, the request parameters) is util/frameFilters;
 * this file is only the interaction.
 */
import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { Icon, Select } from "@/design";
import {
  availableFields,
  chipLabel,
  EXPOSURE_OPS,
  fieldLabel,
  filterProblem,
  isActive,
  isRangeOp,
  newFilter,
  type FilterField,
  type FrameFilter,
} from "../util/frameFilters";
import "./filter-bar.css";

export interface FilterOption {
  value: string;
  label: string;
}

export interface FilterBarProps {
  filters: FrameFilter[];
  onChange: (filters: FrameFilter[]) => void;
  /** The pickable values of each list field (device, filter, type). */
  options: Record<"device" | "filter" | "type", FilterOption[]>;
}

/** Closes a popover on Escape or a pointer press outside `root`. */
function useDismiss(open: boolean, root: React.RefObject<HTMLElement | null>, close: () => void) {
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        close();
      }
    };
    const onDown = (e: PointerEvent) => {
      if (root.current && !root.current.contains(e.target as Node)) close();
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("pointerdown", onDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("pointerdown", onDown);
    };
  }, [open, root, close]);
}

export function FilterBar({ filters, onChange, options }: FilterBarProps) {
  const [editing, setEditing] = useState<FilterField | null>(null);
  const [menuOpen, setMenuOpen] = useState(false);
  const remaining = availableFields(filters);

  const update = (next: FrameFilter) =>
    onChange(filters.map((f) => (f.field === next.field ? next : f)));
  const remove = (field: FilterField) => {
    onChange(filters.filter((f) => f.field !== field));
    if (editing === field) setEditing(null);
  };
  const closeEditor = () => {
    const f = filters.find((x) => x.field === editing);
    // An empty filter left behind would read as narrowing something it doesn't.
    if (f && !isActive(f) && !filterProblem(f)) remove(f.field);
    setEditing(null);
  };
  const add = (field: FilterField) => {
    setMenuOpen(false);
    onChange([...filters, newFilter(field)]);
    setEditing(field);
  };

  const labelOf = (field: "device" | "filter" | "type") => (v: string) =>
    options[field].find((o) => o.value === v)?.label ?? v;

  return (
    <div className="filter-bar" role="group" aria-label="Frame filters">
      {filters.map((f) => {
        const text = chipLabel(
          f,
          f.field === "device" || f.field === "filter" || f.field === "type"
            ? labelOf(f.field)
            : undefined,
        );
        return (
          <Chip
            key={f.field}
            label={text}
            muted={!isActive(f)}
            open={editing === f.field}
            onOpen={() => setEditing(f.field)}
            onClose={closeEditor}
            onRemove={() => remove(f.field)}
          >
            <Editor filter={f} onChange={update} options={options} />
          </Chip>
        );
      })}
      {remaining.length > 0 && (
        <AddMenu open={menuOpen} setOpen={setMenuOpen} fields={remaining} onPick={add} />
      )}
    </div>
  );
}

function Chip(props: {
  label: string;
  muted: boolean;
  open: boolean;
  onOpen: () => void;
  onClose: () => void;
  onRemove: () => void;
  children: ReactNode;
}) {
  const root = useRef<HTMLDivElement>(null);
  const popId = useId();
  useDismiss(props.open, root, props.onClose);
  useEffect(() => {
    if (!props.open) return;
    root.current
      ?.querySelector<HTMLElement>(".filter-pop input, .filter-pop select")
      ?.focus();
  }, [props.open]);

  return (
    <div className={`filter-chip${props.muted ? " filter-chip--muted" : ""}`} ref={root}>
      <button
        type="button"
        className="filter-chip__label"
        aria-expanded={props.open}
        aria-controls={popId}
        onClick={() => (props.open ? props.onClose() : props.onOpen())}
      >
        {props.label}
      </button>
      <button
        type="button"
        className="filter-chip__remove"
        aria-label={`Remove filter: ${props.label}`}
        onClick={props.onRemove}
      >
        <Icon name="close" size={14} />
      </button>
      {props.open && (
        <div className="filter-pop" id={popId} role="dialog" aria-label={props.label}>
          {props.children}
        </div>
      )}
    </div>
  );
}

function AddMenu(props: {
  open: boolean;
  setOpen: (open: boolean) => void;
  fields: FilterField[];
  onPick: (field: FilterField) => void;
}) {
  const root = useRef<HTMLDivElement>(null);
  const menuId = useId();
  useDismiss(props.open, root, () => props.setOpen(false));
  useEffect(() => {
    if (props.open) root.current?.querySelector<HTMLElement>("[role=menuitem]")?.focus();
  }, [props.open]);

  // Up/Down move between items, as a menu's keyboard contract expects.
  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    e.preventDefault();
    const items = [...(root.current?.querySelectorAll<HTMLElement>("[role=menuitem]") ?? [])];
    const i = items.indexOf(document.activeElement as HTMLElement);
    const next = e.key === "ArrowDown" ? (i + 1) % items.length : (i - 1 + items.length) % items.length;
    items[next]?.focus();
  };

  return (
    <div className="filter-add" ref={root}>
      <button
        type="button"
        className="filter-chip filter-chip--add"
        aria-haspopup="menu"
        aria-expanded={props.open}
        aria-controls={menuId}
        onClick={() => props.setOpen(!props.open)}
      >
        <Icon name="add" size={14} />
        <span>Add filter</span>
      </button>
      {props.open && (
        <div className="filter-pop filter-menu" id={menuId} role="menu" onKeyDown={onKeyDown}>
          {props.fields.map((field) => (
            <button
              key={field}
              type="button"
              role="menuitem"
              className="filter-menu__item"
              onClick={() => props.onPick(field)}
            >
              {fieldLabel(field)}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function Editor({
  filter: f,
  onChange,
  options,
}: {
  filter: FrameFilter;
  onChange: (f: FrameFilter) => void;
  options: FilterBarProps["options"];
}) {
  const id = useId();
  const problem = filterProblem(f);
  let body: ReactNode;

  switch (f.field) {
    case "target":
    case "path":
      body = (
        <label className="filter-pop__row">
          <span className="text-sm text-muted">{fieldLabel(f.field)} contains</span>
          <input
            className="aac-input"
            value={f.text}
            onChange={(e) => onChange({ ...f, text: e.target.value })}
            placeholder={f.field === "target" ? "e.g. NGC 1499" : "e.g. Flats"}
          />
        </label>
      );
      break;
    case "device":
    case "filter":
    case "type": {
      const list = options[f.field];
      const toggle = (v: string) =>
        onChange({
          ...f,
          values: f.values.includes(v) ? f.values.filter((x) => x !== v) : [...f.values, v],
        });
      body = (
        <>
          <Select
            aria-label={`${fieldLabel(f.field)} comparison`}
            value={f.not ? "not" : "is"}
            onChange={(e) => onChange({ ...f, not: e.target.value === "not" })}
            options={[
              { value: "is", label: "is" },
              { value: "not", label: "is not" },
            ]}
          />
          {list.length === 0 ? (
            <p className="text-sm text-muted">No values in the transfer history yet.</p>
          ) : (
            <fieldset className="filter-pop__list">
              <legend className="sr-only">{fieldLabel(f.field)} values</legend>
              {list.map((o) => (
                <label key={o.value} className="filter-pop__check">
                  <input
                    type="checkbox"
                    className="aac-checkbox"
                    checked={f.values.includes(o.value)}
                    onChange={() => toggle(o.value)}
                  />
                  <span className={o.value === "" ? "mono" : undefined}>{o.label}</span>
                </label>
              ))}
            </fieldset>
          )}
        </>
      );
      break;
    }
    case "date":
      body = (
        <>
          <Select
            aria-label="Date comparison"
            value={f.not ? "not" : "in"}
            onChange={(e) => onChange({ ...f, not: e.target.value === "not" })}
            options={[
              { value: "in", label: "in" },
              { value: "not", label: "not in" },
            ]}
          />
          <label className="filter-pop__row" htmlFor={`${id}-from`}>
            <span className="text-sm text-muted">From (included)</span>
          </label>
          <input
            id={`${id}-from`}
            type="date"
            className="aac-input"
            value={f.from}
            onChange={(e) => onChange({ ...f, from: e.target.value })}
          />
          <label className="filter-pop__row" htmlFor={`${id}-to`}>
            <span className="text-sm text-muted">Until (excluded)</span>
          </label>
          <input
            id={`${id}-to`}
            type="date"
            className="aac-input"
            value={f.to}
            onChange={(e) => onChange({ ...f, to: e.target.value })}
          />
          <p className="text-sm text-muted">Either end may be left open. Local time.</p>
        </>
      );
      break;
    case "exposure":
      body = (
        <>
          <Select
            aria-label="Exposure comparison"
            value={f.op}
            onChange={(e) => onChange({ ...f, op: e.target.value as typeof f.op })}
            options={EXPOSURE_OPS.map((o) => ({ value: o.op, label: o.label }))}
          />
          <div className="filter-pop__nums">
            <input
              className="aac-input"
              inputMode="decimal"
              aria-label={isRangeOp(f.op) ? "From, seconds" : "Seconds"}
              placeholder="seconds"
              value={f.min}
              onChange={(e) => onChange({ ...f, min: e.target.value })}
            />
            {isRangeOp(f.op) && (
              <>
                <span className="text-sm text-muted">to</span>
                <input
                  className="aac-input"
                  inputMode="decimal"
                  aria-label="To, seconds"
                  placeholder="seconds"
                  value={f.max}
                  onChange={(e) => onChange({ ...f, max: e.target.value })}
                />
              </>
            )}
          </div>
          {isRangeOp(f.op) && (
            <p className="text-sm text-muted">Both ends included.</p>
          )}
        </>
      );
      break;
  }

  return (
    <div className="filter-pop__body">
      {body}
      {problem && (
        <p className="text-sm tone-problem" role="alert">
          {problem}
        </p>
      )}
    </div>
  );
}
