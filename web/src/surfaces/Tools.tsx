/**
 * Tools — a page for operator utilities on already-archived frames. Edit header
 * (design header-edits §9.3) corrects one FITS keyword on archived frames: the
 * target (OBJECT) an ASIAIR Autorun recorded wrongly at capture time, which
 * re-files the frame under the corrected target (design routing-and-filing §9),
 * or the filter (FILTER), which re-files it only when its folder template names
 * the filter. Re-file moves correctly labelled frames to where the folder
 * templates say they belong.
 *
 * The frame list reuses the cross-rig transfer-history read path
 * (api.listTransferHistory) rather than a dedicated list endpoint. That path has
 * no target-substring or date_obs range filter, so the date range and target
 * filters here narrow the loaded page client-side, and the FILTER suggestions
 * come from that same page. Paths shown by Preview are the intended destination;
 * a real name collision at Apply time may disambiguate the filename slightly.
 *
 * Selection may span rigs (the cross-rig list already mixes them); Preview and
 * Apply each group the selection by rig_id and issue one request per rig, since
 * both edit endpoints are per-rig (`/api/rigs/{id}/edit/...`).
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router";
import {
  api,
  applyEdit,
  applyRefile,
  ApiError,
  previewEdit,
  previewRefile,
  type EditableKey,
  type EditPreview,
  type EditResult,
  type Frame,
  type RefileGroup,
  type RefilePreview,
  type RefileResult,
  type RefileScope,
  type RefileStatus,
  type RigProfile,
  type StrandRisk,
} from "@/api";
import {
  Button,
  Card,
  Dialog,
  EmptyState,
  Field,
  Icon,
  Select,
  Spinner,
  StatusPill,
  Table,
  TextInput,
  editResultView,
  useToast,
  type Column,
} from "@/design";
import { basename } from "../util/format";
import { formatExposure } from "../util/exposure";
import {
  EDIT_FIELDS,
  EDITABLE_KEYS,
  filterSuggestions,
  validateEditValue,
} from "../util/headerEdit";
import "./surfaces.css";
import "./tools.css";

const PAGE_SIZE = 200; // the backend's historyMaxPageSize clamp

/** A YYYY-MM-DD date input value → unix millis at day start/end, or undefined when blank. */
function dayStartMs(value: string): number | undefined {
  if (!value) return undefined;
  const t = Date.parse(`${value}T00:00:00`);
  return Number.isNaN(t) ? undefined : t;
}
function dayEndMs(value: string): number | undefined {
  if (!value) return undefined;
  const t = Date.parse(`${value}T23:59:59`);
  return Number.isNaN(t) ? undefined : t;
}

/** The directory portion of a filed path, for the "current folder" column. */
function dirname(path: string): string {
  if (!path) return "";
  const idx = Math.max(path.lastIndexOf("/"), path.lastIndexOf("\\"));
  return idx >= 0 ? path.slice(0, idx) : "";
}

/** What a key's value is called in running copy. */
const KEY_NOUN: Record<EditableKey, string> = { OBJECT: "target", FILTER: "filter" };

/**
 * The spellings a preview row reports (filed in the destination folder for a
 * target, elsewhere on the rig for a filter), as one readable phrase. There can
 * be more than a handful, and the point is that there is divergence at all — so
 * three are named and the rest counted.
 */
function spellingList(names: string[]): string {
  const shown = names.slice(0, 3).map((n) => `"${n}"`);
  const rest = names.length - shown.length;
  const head = shown.join(", ");
  return rest > 0 ? `${head} and ${rest} more` : head;
}

function groupByRig(frames: Frame[]): Map<string, Frame[]> {
  const out = new Map<string, Frame[]>();
  for (const f of frames) {
    const list = out.get(f.rig_id);
    if (list) list.push(f);
    else out.set(f.rig_id, [f]);
  }
  return out;
}

export function Tools() {
  const toast = useToast();

  const [rigs, setRigs] = useState<RigProfile[]>([]);
  // The device column falls back to the raw id when rigs can't load, but re-file
  // can't: it needs a rig to address, so the failure is surfaced there.
  const [rigsError, setRigsError] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    api
      .listRigs()
      .then((list) => !cancelled && setRigs(list))
      .catch((err) => {
        if (cancelled) return;
        setRigsError(
          err instanceof ApiError ? err.message : "Couldn't load the device list.",
        );
      });
    return () => {
      cancelled = true;
    };
  }, []);
  const rigName = useCallback(
    (id: string) => rigs.find((r) => r.id === id)?.name || id,
    [rigs],
  );

  // ---- frame list (cross-rig, paginated) ----------------------------------
  const [page, setPage] = useState(1);
  const [pageFrames, setPageFrames] = useState<Frame[] | null>(null);
  const [total, setTotal] = useState(0);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Set<number>>(new Set());

  const inflight = useRef<AbortController | null>(null);
  const reload = useCallback(() => {
    inflight.current?.abort();
    const ctrl = new AbortController();
    inflight.current = ctrl;
    setLoadError(null);
    api
      .listTransferHistory({ page, page_size: PAGE_SIZE }, ctrl.signal)
      .then((res) => {
        if (ctrl.signal.aborted) return;
        setPageFrames(res.frames);
        setTotal(res.total);
        setSelected(new Set());
      })
      .catch((err) => {
        if (err instanceof DOMException && err.name === "AbortError") return;
        setLoadError(err instanceof ApiError ? err.message : "Couldn't load frames.");
        setPageFrames([]);
      });
  }, [page]);

  useEffect(() => {
    setPageFrames(null);
    reload();
    return () => inflight.current?.abort();
  }, [reload]);

  // ---- filter bar (client-side, over the loaded page — see file header) --
  const [targetFilter, setTargetFilter] = useState("");
  const [fromDate, setFromDate] = useState("");
  const [toDate, setToDate] = useState("");

  const filtered = useMemo(() => {
    const frames = pageFrames ?? [];
    const needle = targetFilter.trim().toLowerCase();
    const fromMs = dayStartMs(fromDate);
    const toMs = dayEndMs(toDate);
    return frames.filter((f) => {
      if (needle && !(f.target ?? "").toLowerCase().includes(needle)) return false;
      if (fromMs !== undefined || toMs !== undefined) {
        const t = f.date_obs ? Date.parse(f.date_obs) : NaN;
        if (Number.isNaN(t)) return false;
        if (fromMs !== undefined && t < fromMs) return false;
        if (toMs !== undefined && t > toMs) return false;
      }
      return true;
    });
  }, [pageFrames, targetFilter, fromDate, toDate]);

  const toggleOne = (id: number) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };
  const selectAll = () =>
    setSelected((prev) => new Set([...prev, ...filtered.map((f) => f.id)]));
  const deselectAll = () =>
    setSelected((prev) => {
      const next = new Set(prev);
      for (const f of filtered) next.delete(f.id);
      return next;
    });

  const selectedFrames = useMemo(
    () => (pageFrames ?? []).filter((f) => selected.has(f.id)),
    [pageFrames, selected],
  );

  // ---- field + value input, live-validated per key -------------------------
  const [editKey, setEditKey] = useState<EditableKey>("OBJECT");
  const [newValue, setNewValue] = useState("");
  const [attempted, setAttempted] = useState(false);
  const field = EDIT_FIELDS[editKey];
  const valueError =
    attempted || newValue.length > 0 ? validateEditValue(editKey, newValue) : undefined;
  const canAct = selectedFrames.length > 0 && !validateEditValue(editKey, newValue);
  const suggestions = useMemo(() => filterSuggestions(pageFrames ?? []), [pageFrames]);

  // A value typed for one keyword is rarely meant for the other, so switching
  // starts the input over rather than carrying a target into the filter.
  const changeKey = (key: EditableKey) => {
    setEditKey(key);
    setNewValue("");
    setAttempted(false);
  };

  // ---- preview -------------------------------------------------------------
  const [previewBusy, setPreviewBusy] = useState(false);
  const [previews, setPreviews] = useState<EditPreview[] | null>(null);
  const [previewFrames, setPreviewFrames] = useState<Frame[]>([]);
  const [previewKey, setPreviewKey] = useState<EditableKey>("OBJECT");
  const [previewValue, setPreviewValue] = useState("");

  const runPreview = useCallback(async () => {
    setAttempted(true);
    if (selectedFrames.length === 0 || validateEditValue(editKey, newValue) || previewBusy) {
      return;
    }
    const key = editKey;
    const value = newValue.trim();
    setPreviewBusy(true);
    const entries = Array.from(groupByRig(selectedFrames).entries());
    const settled = await Promise.allSettled(
      entries.map(([rigId, frames]) =>
        previewEdit(rigId, frames.map((f) => f.id), key, value),
      ),
    );
    const merged: EditPreview[] = [];
    let failures = 0;
    settled.forEach((r, i) => {
      if (r.status === "fulfilled") {
        merged.push(...r.value);
      } else {
        failures++;
        const rigId = entries[i]![0];
        toast.error(
          r.reason instanceof ApiError ? r.reason.message : "Couldn't reach the server.",
          `Preview failed — ${rigName(rigId)}`,
        );
      }
    });
    setPreviews(merged);
    setPreviewFrames(selectedFrames);
    setPreviewKey(key);
    setPreviewValue(value);
    setPreviewBusy(false);
    if (failures === 0 && merged.length > 0) {
      const ineligible = merged.filter((p) => !p.eligible).length;
      toast.info(
        ineligible > 0
          ? `${merged.length - ineligible} of ${merged.length} frames can be edited; ${ineligible} ineligible.`
          : `All ${merged.length} frames can be edited.`,
      );
    }
  }, [editKey, newValue, selectedFrames, previewBusy, toast, rigName]);

  const previewFrameById = useMemo(() => {
    const m = new Map<number, Frame>();
    for (const f of previewFrames) m.set(f.id, f);
    return m;
  }, [previewFrames]);

  /** How many of the selected frames Apply would actually edit, or null when
   * that is unknown — no preview yet, a preview whose per-rig requests didn't all
   * answer, or one taken against a different selection, keyword or value. The
   * confirmation states blast radius, so it may only quote a count the preview
   * still covers. */
  const eligibleCount = useMemo(() => {
    if (!previews || previewKey !== editKey || previewValue !== newValue.trim()) return null;
    if (previews.length !== selectedFrames.length) return null;
    const covered = new Set(previews.map((p) => p.frame_id));
    if (!selectedFrames.every((f) => covered.has(f.id))) return null;
    return previews.filter((p) => p.eligible).length;
  }, [previews, previewKey, previewValue, editKey, newValue, selectedFrames]);

  // Without a covering preview the confirmation quotes the selection as a ceiling
  // ("up to N"), never as a promise, because the skipped share is unknown.
  const confirmCount = eligibleCount ?? selectedFrames.length;
  const confirmSkipped = eligibleCount === null ? 0 : selectedFrames.length - eligibleCount;
  const noun = KEY_NOUN[editKey];
  const confirmDetail =
    eligibleCount === null
      ? `Frames already carrying that ${noun}, or otherwise ineligible, are skipped — preview to see the exact count.`
      : confirmSkipped > 0
        ? `The other ${confirmSkipped} selected frame${confirmSkipped === 1 ? " is" : "s are"} skipped — already on that ${noun}, or otherwise ineligible.`
        : null;

  // ---- apply (streamed, per-rig, live progress) ----------------------------
  const [applyConfirmOpen, setApplyConfirmOpen] = useState(false);
  const [applyBusy, setApplyBusy] = useState(false);
  const [applyFrames, setApplyFrames] = useState<Frame[]>([]);
  const [applyResults, setApplyResults] = useState<Map<number, EditResult>>(new Map());

  const openApplyConfirm = () => {
    setAttempted(true);
    if (!canAct) return;
    setApplyConfirmOpen(true);
  };

  const runApply = useCallback(async () => {
    const key = editKey;
    const value = newValue.trim();
    const frames = selectedFrames;
    setApplyConfirmOpen(false);
    if (frames.length === 0) return;

    setApplyBusy(true);
    setApplyFrames(frames);
    setApplyResults(new Map());

    const localResults = new Map<number, EditResult>();
    const entries = Array.from(groupByRig(frames).entries());
    const settled = await Promise.allSettled(
      entries.map(([rigId, rigFrames]) =>
        applyEdit(rigId, rigFrames.map((f) => f.id), key, value, (r) => {
          localResults.set(r.frame_id, r);
          setApplyResults(new Map(localResults));
        }),
      ),
    );

    let rigFailures = 0;
    settled.forEach((r, i) => {
      if (r.status === "rejected") {
        rigFailures++;
        const rigId = entries[i]![0];
        toast.error(
          r.reason instanceof ApiError ? r.reason.message : "Couldn't reach the server.",
          `Edit failed — ${rigName(rigId)}`,
        );
      }
    });

    setApplyBusy(false);
    setPreviews(null);
    reload(); // edited frames carry the new value and possibly a new dest_path

    let edited = 0;
    let skipped = 0;
    let failed = 0;
    for (const r of localResults.values()) {
      if (r.status === "edited") edited++;
      else if (r.status === "skipped") skipped++;
      else failed++;
    }
    if (rigFailures === 0) {
      toast.success(
        `Set ${key} to "${value}" on ${edited} of ${frames.length}` +
          (skipped || failed ? ` (${skipped} skipped, ${failed} failed).` : "."),
        skipped || failed ? undefined : "Edit complete",
      );
    }
  }, [editKey, newValue, selectedFrames, toast, rigName, reload]);

  const columns: Column<Frame>[] = [
    {
      key: "select",
      header: <span className="sr-only">Select</span>,
      cell: (f) => (
        <input
          type="checkbox"
          className="aac-checkbox"
          checked={selected.has(f.id)}
          onChange={() => toggleOne(f.id)}
          disabled={applyBusy}
          aria-label={`Select ${f.target || basename(f.source_path)}`}
        />
      ),
    },
    { key: "device", header: "Device", cell: (f) => rigName(f.rig_id) },
    { key: "target", header: "Target", cell: (f) => f.target || "—" },
    { key: "filter", header: "Filter", cell: (f) => f.filter || "—" },
    {
      key: "exptime",
      header: "Exposure",
      cell: (f) => (f.exptime ? formatExposure(f.exptime) : "—"),
      numeric: true,
    },
    {
      key: "date_obs",
      header: "Date observed",
      cell: (f) => <span className="mono text-sm">{f.date_obs || "—"}</span>,
    },
    {
      key: "folder",
      header: "Current folder",
      cell: (f) => (
        <span className="mono text-sm break-anywhere">{dirname(f.dest_path ?? "") || "—"}</span>
      ),
      width: 260,
    },
  ];

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  return (
    <>
      <header className="surface-head">
        <div>
          <h1 className="surface-head__title">Tools</h1>
          <p className="surface-head__desc">
            Utilities for already-archived frames. Edit header corrects the target or
            filter recorded in a frame's FITS header — a wrong Autorun target, or a
            filter name your stacking software groups differently — and refiles the
            frame when its folder changes. Re-file moves frames that are already
            labelled correctly to where your folder templates say they belong. Device
            originals are never touched by either.
          </p>
        </div>
      </header>

      <Card pad style={{ marginBottom: "var(--space-5)" }}>
        <h2 className="section-title">Find frames</h2>
        <p className="section-sub">
          Filters narrow the {PAGE_SIZE.toLocaleString()} transfers loaded on this
          page; use Previous/Next below to reach older ones.
        </p>

        <div className="toolbar">
          <label className="text-sm text-muted" htmlFor="tools-target-filter">
            Target contains
          </label>
          <input
            id="tools-target-filter"
            className="aac-input"
            style={{ maxWidth: 220 }}
            value={targetFilter}
            onChange={(e) => setTargetFilter(e.target.value)}
            placeholder="e.g. Moon"
          />

          <label className="text-sm text-muted" htmlFor="tools-from">
            From
          </label>
          <input
            id="tools-from"
            type="date"
            className="aac-input"
            style={{ maxWidth: 170 }}
            value={fromDate}
            max={toDate || undefined}
            onChange={(e) => setFromDate(e.target.value)}
          />
          <label className="text-sm text-muted" htmlFor="tools-to">
            To
          </label>
          <input
            id="tools-to"
            type="date"
            className="aac-input"
            style={{ maxWidth: 170 }}
            value={toDate}
            min={fromDate || undefined}
            onChange={(e) => setToDate(e.target.value)}
          />
        </div>

        {loadError ? (
          <p className="notice tone-problem" role="alert">
            <Icon name="problem" size={18} />
            <span>{loadError}</span>
          </p>
        ) : !pageFrames ? (
          <div style={{ display: "grid", placeItems: "center", minHeight: "20vh" }}>
            <Spinner label="Loading frames…" />
          </div>
        ) : (
          <>
            <div className="cluster-between" style={{ marginBottom: "var(--space-3)" }}>
              <span className="text-sm text-muted" aria-live="polite">
                {selected.size} selected of {filtered.length} shown
              </span>
              <div className="cluster">
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={selectAll}
                  disabled={applyBusy || filtered.length === 0}
                >
                  Select all
                </Button>
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={deselectAll}
                  disabled={applyBusy || selected.size === 0}
                >
                  Deselect all
                </Button>
              </div>
            </div>

            <Table
              caption="Frames available for header edits"
              columns={columns}
              rows={filtered}
              getKey={(f) => f.id}
              empty={
                <EmptyState
                  icon="nas"
                  title="No frames match these filters"
                  description="Widen the date range, clear the target filter, or turn the page to see more."
                />
              }
            />

            {total > PAGE_SIZE && (
              <nav className="pager" aria-label="Frame pages">
                <Button
                  size="sm"
                  variant="secondary"
                  disabled={page <= 1}
                  onClick={() => setPage((p) => Math.max(1, p - 1))}
                >
                  Previous
                </Button>
                <span className="pager__status" aria-live="polite">
                  Page {page} of {totalPages}
                </span>
                <Button
                  size="sm"
                  variant="secondary"
                  disabled={page >= totalPages}
                  onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
                >
                  Next
                </Button>
              </nav>
            )}
          </>
        )}
      </Card>

      <Card pad style={{ marginBottom: "var(--space-5)" }}>
        <h2 className="section-title">Edit header</h2>
        <div className="form-grid" style={{ marginTop: "var(--space-3)" }}>
          <Field label="Header field" help="The FITS keyword to change on the selected frames.">
            {(f) => (
              <Select
                {...f}
                value={editKey}
                disabled={applyBusy}
                onChange={(e) => changeKey(e.target.value as EditableKey)}
                options={EDITABLE_KEYS.map((k) => ({ value: k, label: EDIT_FIELDS[k].option }))}
              />
            )}
          </Field>
          <Field
            label={field.inputLabel}
            help={field.help}
            example={field.example}
            error={valueError}
            required
          >
            {(f) => (
              <TextInput
                {...f}
                value={newValue}
                onChange={(e) => setNewValue(e.target.value)}
                placeholder={field.placeholder}
                disabled={applyBusy}
                invalid={!!valueError}
                list={editKey === "FILTER" ? "tools-filter-suggestions" : undefined}
                autoComplete="off"
              />
            )}
          </Field>
          {editKey === "FILTER" && (
            <datalist id="tools-filter-suggestions">
              {suggestions.map((s) => (
                <option key={s} value={s} />
              ))}
            </datalist>
          )}
        </div>

        <div className="cluster" style={{ marginTop: "var(--space-4)" }}>
          <Button
            variant="secondary"
            icon={<Icon name="scan" size={16} />}
            loading={previewBusy}
            disabled={!canAct || applyBusy}
            onClick={runPreview}
          >
            Preview
          </Button>
          <Button
            variant="primary"
            icon={<Icon name="reclassify" size={16} />}
            disabled={!canAct || applyBusy}
            onClick={openApplyConfirm}
          >
            Apply
          </Button>
        </div>
      </Card>

      {previews && (
        <Card pad style={{ marginBottom: "var(--space-5)" }}>
          <h2 className="section-title">Preview</h2>
          <p className="section-sub">
            Paths shown are the intended destination — Apply may adjust the filename
            slightly if it collides with an existing file.
          </p>
          <ul className="list-reset header-edit-preview">
            {previews.map((p) => {
              const f = previewFrameById.get(p.frame_id);
              return (
                <li
                  key={p.frame_id}
                  className={`header-edit-preview__item${
                    p.eligible ? "" : " header-edit-preview__item--ineligible"
                  }`}
                >
                  <div className="header-edit-preview__head">
                    <span className="text-sm text-muted">
                      {f ? rigName(f.rig_id) : ""} — {previewKey}{" "}
                      {p.current_present ? (
                        <span className="mono">"{p.current_value}"</span>
                      ) : (
                        "none"
                      )}
                    </span>
                    {!p.eligible && <StatusPill tone="problem" icon="problem" label="Ineligible" />}
                  </div>
                  {p.in_place ? (
                    <>
                      <div className="header-edit-preview__paths mono text-sm break-anywhere">
                        <span>{p.old_path}</span>
                      </div>
                      <p className="text-xs text-muted">
                        The file keeps its path — only its header changes.
                      </p>
                    </>
                  ) : (
                    <div className="header-edit-preview__paths mono text-sm break-anywhere">
                      <span>{p.old_path}</span>
                      <Icon name="chevron-right" size={14} />
                      <span>{p.new_path}</span>
                    </div>
                  )}
                  {p.night_approx && (
                    <p className="text-xs text-muted">
                      Night number may change — another frame filed before you apply can
                      shift it.
                    </p>
                  )}
                  {p.existing_spellings &&
                    p.existing_spellings.length > 0 &&
                    (previewKey === "OBJECT" ? (
                      <p className="text-xs text-muted">
                        Already filed here as {spellingList(p.existing_spellings)} — applying
                        will leave two spellings of this target in one folder.
                      </p>
                    ) : (
                      // Case is not folded for a filter (design header-edits §9.1), so a
                      // spelling that differs only in case is a likely typo worth flagging
                      // before Apply, not a refusal.
                      <p className="notice tone-working header-edit-preview__warning">
                        <Icon name="alert" size={16} />
                        <span>
                          This device already has {spellingList(p.existing_spellings)}, which
                          differs from "{previewValue}" only in case. Stacking software may
                          treat them as separate filters.
                        </span>
                      </p>
                    ))}
                  {p.reason &&
                    (p.eligible ? (
                      // An eligible row's reason is informational — a read the preview
                      // could not make — not a refusal, so it is not styled as a problem.
                      <p className="text-xs text-muted">{p.reason}</p>
                    ) : (
                      <p className="text-xs" style={{ color: "var(--problem-fg)" }}>
                        {p.reason}
                      </p>
                    ))}
                </li>
              );
            })}
          </ul>
        </Card>
      )}

      {applyFrames.length > 0 && (
        <Card pad>
          <h2 className="section-title">Apply progress</h2>
          <p className="section-sub" aria-live="polite">
            {applyBusy
              ? `Editing ${applyResults.size} of ${applyFrames.length}…`
              : `Done — ${applyFrames.length} frame${applyFrames.length === 1 ? "" : "s"} processed.`}
          </p>
          <ul className="list-reset header-edit-progress">
            {applyFrames.map((f) => {
              const r = applyResults.get(f.id);
              return (
                <li key={f.id} className="header-edit-progress__item">
                  <span className="mono text-sm break-anywhere grow">
                    {basename(f.dest_path || f.source_path)}
                  </span>
                  {r ? (
                    <>
                      <StatusPill view={editResultView(r.status)} />
                      {r.reason && <span className="text-xs text-muted">{r.reason}</span>}
                    </>
                  ) : (
                    <Spinner size={16} label="Applying…" />
                  )}
                </li>
              );
            })}
          </ul>
        </Card>
      )}

      <RefilePanel rigs={rigs} rigsError={rigsError} onApplied={reload} />

      <Dialog
        open={applyConfirmOpen}
        onClose={() => setApplyConfirmOpen(false)}
        title={`Edit ${editKey} on frames?`}
        footer={
          <>
            <Button variant="ghost" onClick={() => setApplyConfirmOpen(false)}>
              Cancel
            </Button>
            {/* A covering preview that found nothing to edit leaves nothing to
                confirm: every frame would come back skipped. */}
            <Button variant="primary" onClick={runApply} disabled={eligibleCount === 0}>
              Edit {eligibleCount === null && "up to "}
              {confirmCount} frame{confirmCount === 1 ? "" : "s"}
            </Button>
          </>
        }
      >
        <p>
          This sets the FITS <span className="mono">{editKey}</span> header to{" "}
          <strong>{newValue.trim()}</strong> on {eligibleCount === null && "up to "}
          <strong>{confirmCount}</strong> archived frame{confirmCount === 1 ? "" : "s"},
          refiling any whose folder changes. {confirmDetail} The original on the device
          is not modified, and each frame's outcome is reported as it completes.
        </p>
      </Dialog>
    </>
  );
}

/* ---- Re-file -------------------------------------------------------------- */

/** How many directories a notice lists before it summarises the rest. */
const MAX_LISTED_DIRS = 12;

/** A group is identified by the PAIR of directories — two source folders can
 * collapse onto one destination — so the destination alone is not a key. */
const groupKey = (g: RefileGroup) => `${g.from_dir}\u0000${g.to_dir}`;

const SCOPE_HELP: Record<RefileScope, string> = {
  mosaic:
    "Only the frames your current settings would file through the mosaic template — the migration a library shot as mosaics needs.",
  all: "Every eligible frame whose folder would change, whatever caused it. Renaming an OTA or a session changes the folder of every frame that rig ever filed, so read the diff before selecting anything.",
};

/**
 * Re-file — move already-archived frames to where the current folder templates say
 * they belong (design routing-and-filing §8.11). Selection is by
 * GROUP (`old dir → new dir`), never by row: a mosaic migration covers hundreds of
 * frames and nobody ticks them one at a time.
 *
 * Both endpoints are per-rig, hence the device selector. Apply runs one group per
 * request and the worker runs a group to completion without yielding, so per-file
 * lines arrive as a burst when each group finishes — progress is per group, and
 * the selected groups are applied in sequence.
 */
function RefilePanel({
  rigs,
  rigsError,
  onApplied,
}: {
  rigs: RigProfile[];
  /** Non-null when the rig list failed to load; re-file needs a rig to address, so
   * unlike the frame table's device column it can't quietly fall back. */
  rigsError: string | null;
  onApplied: () => void;
}) {
  const toast = useToast();

  const [rigId, setRigId] = useState("");
  useEffect(() => {
    if (!rigId && rigs.length > 0) setRigId(rigs[0]!.id);
  }, [rigs, rigId]);
  const rig = rigs.find((r) => r.id === rigId);

  // The mosaic scope is only meaningful while mosaic sorting is on: with it off no
  // frame routes through the mosaic template, so the scope is empty by
  // construction. null means "not read yet".
  const [mosaicDetection, setMosaicDetection] = useState<boolean | null>(null);
  const [configError, setConfigError] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    api
      .getConfig()
      .then((cfg) => !cancelled && setMosaicDetection(cfg.mosaic_detection))
      .catch((err) => {
        if (cancelled) return;
        setConfigError(
          err instanceof ApiError ? err.message : "Couldn't read your settings.",
        );
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const [scope, setScope] = useState<RefileScope>("mosaic");
  useEffect(() => {
    if (mosaicDetection === false) setScope("all");
  }, [mosaicDetection]);

  // ---- preview -------------------------------------------------------------
  const [preview, setPreview] = useState<RefilePreview | null>(null);
  const [previewBusy, setPreviewBusy] = useState(false);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());

  // A diff belongs to the rig and scope it was computed for.
  useEffect(() => {
    setPreview(null);
    setPreviewError(null);
    setSelected(new Set());
  }, [rigId, scope]);

  const runPreview = useCallback(async () => {
    if (!rigId || previewBusy) return;
    setPreviewBusy(true);
    setPreviewError(null);
    // A fresh diff supersedes the last run's outcome, which described a library
    // state this preview has just re-measured.
    setProgress(null);
    try {
      const pv = await previewRefile(rigId, scope);
      setPreview(pv);
      setSelected(new Set());
    } catch (err) {
      setPreview(null);
      setPreviewError(
        err instanceof ApiError ? err.message : "Couldn't reach the server.",
      );
    } finally {
      setPreviewBusy(false);
    }
  }, [rigId, scope, previewBusy]);

  // ---- apply (one request per group, in sequence) --------------------------
  const [applyBusy, setApplyBusy] = useState(false);
  const [progress, setProgress] = useState<{
    done: number;
    total: number;
    group: RefileGroup | null;
  } | null>(null);
  const [counts, setCounts] = useState<Record<RefileStatus, number>>({
    moved: 0,
    skipped: 0,
    failed: 0,
  });
  const [failures, setFailures] = useState<RefileResult[]>([]);
  const [prunedDirs, setPrunedDirs] = useState<string[]>([]);
  const [skippedDirs, setSkippedDirs] = useState<StrandRisk[]>([]);
  const [stopped, setStopped] = useState(false);
  const abort = useRef<AbortController | null>(null);
  useEffect(() => () => abort.current?.abort(), []);

  const groups = preview?.groups ?? [];
  const refusals = preview?.refusals ?? [];
  const selectedGroups = groups.filter((g) => selected.has(groupKey(g)));
  const selectedFiles = selectedGroups.reduce((n, g) => n + g.count, 0);

  // Every directory the preview expects the prune to be unable to remove, once
  // each: on a macOS-mounted NAS a .DS_Store strands most vacated folders, and
  // that is worth knowing before the move rather than after it.
  const stranded = useMemo(() => {
    const seen = new Map<string, StrandRisk>();
    for (const g of preview?.groups ?? []) {
      for (const s of g.strand_risk ?? []) if (!seen.has(s.dir)) seen.set(s.dir, s);
    }
    return Array.from(seen.values());
  }, [preview]);

  const runApply = useCallback(async () => {
    const chosen = (preview?.groups ?? []).filter((g) => selected.has(groupKey(g)));
    if (!rigId || chosen.length === 0 || applyBusy) return;

    const ctrl = new AbortController();
    abort.current = ctrl;
    const tally: Record<RefileStatus, number> = { moved: 0, skipped: 0, failed: 0 };
    const failed: RefileResult[] = [];
    const pruned: string[] = [];
    const skipped: StrandRisk[] = [];
    setApplyBusy(true);
    setStopped(false);
    setCounts({ ...tally });
    setFailures([]);
    setPrunedDirs([]);
    setSkippedDirs([]);

    let done = 0;
    let interrupted = false;
    for (const g of chosen) {
      setProgress({ done, total: chosen.length, group: g });
      try {
        const summary = await applyRefile(
          rigId,
          g.to_dir,
          g.frame_ids,
          (r) => {
            tally[r.status] += 1;
            if (r.status !== "moved") failed.push(r);
          },
          ctrl.signal,
        );
        if (summary) {
          pruned.push(...(summary.pruned_dirs ?? []));
          skipped.push(...(summary.skipped_dirs ?? []));
        }
        done++;
      } catch (err) {
        interrupted = true;
        setStopped(true);
        if (!(err instanceof DOMException && err.name === "AbortError")) {
          toast.error(
            err instanceof ApiError ? err.message : "Couldn't reach the server.",
            "Re-file stopped",
          );
        }
        break;
      } finally {
        // Flushed per group rather than per line: the lines of one group arrive
        // together anyway, and a group that threw part-way still reported some.
        setCounts({ ...tally });
        setFailures([...failed]);
        setPrunedDirs([...pruned]);
        setSkippedDirs([...skipped]);
        setProgress({ done, total: chosen.length, group: null });
      }
    }

    abort.current = null;
    setApplyBusy(false);
    // The diff describes a library that has just changed, and a move emits no
    // event — so drop it and reload the frame list, which is still serving the
    // old paths.
    setPreview(null);
    setSelected(new Set());
    onApplied();
    if (!interrupted) {
      toast.success(
        `Moved ${tally.moved} file${tally.moved === 1 ? "" : "s"}` +
          (tally.skipped || tally.failed
            ? ` (${tally.skipped} skipped, ${tally.failed} failed).`
            : "."),
        "Re-file complete",
      );
    }
  }, [preview, selected, rigId, applyBusy, toast, onApplied]);

  const toggleGroup = (g: RefileGroup) =>
    setSelected((prev) => {
      const next = new Set(prev);
      const k = groupKey(g);
      if (next.has(k)) next.delete(k);
      else next.add(k);
      return next;
    });

  return (
    <Card pad style={{ marginBottom: "var(--space-5)" }}>
      <h2 className="section-title">Re-file to the current templates</h2>
      <p className="section-sub">
        Moves already-archived frames to where your current folder templates say they
        belong, and removes the folders they leave empty. Only the file's location
        changes — its contents, its night number and its device original are
        untouched. This is the supported way to reorganise the library; moving the
        folders by hand looks like data loss to Commander.
      </p>

      {rigsError && (
        <p className="notice tone-problem" role="alert" style={{ marginTop: "var(--space-3)" }}>
          <Icon name="problem" size={18} />
          <span>{rigsError} Re-file needs the device list, so try reloading.</span>
        </p>
      )}

      <div className="toolbar">
        <label className="text-sm text-muted" htmlFor="refile-rig">
          Device
        </label>
        <Select
          id="refile-rig"
          style={{ maxWidth: 240 }}
          value={rigId}
          disabled={applyBusy || rigs.length === 0}
          onChange={(e) => setRigId(e.target.value)}
          options={rigs.map((r) => ({ value: r.id, label: r.name || r.id }))}
        />

        <label className="text-sm text-muted" htmlFor="refile-scope">
          Scope
        </label>
        <Select
          id="refile-scope"
          style={{ maxWidth: 320 }}
          value={scope}
          disabled={applyBusy}
          onChange={(e) => setScope(e.target.value as RefileScope)}
        >
          <option value="mosaic" disabled={mosaicDetection === false}>
            Mosaic panels{mosaicDetection === false ? " — mosaic sorting is off" : ""}
          </option>
          <option value="all">Every frame whose folder would change</option>
        </Select>

        <Button
          variant="secondary"
          icon={<Icon name="scan" size={16} />}
          loading={previewBusy}
          disabled={!rigId || applyBusy}
          onClick={runPreview}
        >
          Preview
        </Button>
      </div>

      <p className="text-sm text-muted">{SCOPE_HELP[scope]}</p>

      {mosaicDetection === false && (
        <p className="notice tone-neutral" style={{ marginTop: "var(--space-3)" }}>
          <Icon name="info" size={18} />
          <span>
            Mosaic sorting is off, so no frame routes through the mosaic template and
            that scope would be empty. Turn on{" "}
            <Link to="/settings">Detect and sort mosaic panels</Link> in Settings to
            use it. Re-file still works for any other template change.
          </span>
        </p>
      )}
      {configError && (
        <p className="notice tone-neutral" style={{ marginTop: "var(--space-3)" }}>
          <Icon name="info" size={18} />
          <span>
            {configError} The mosaic scope stays unavailable until your settings can
            be read.
          </span>
        </p>
      )}

      {rig && (
        <p className="text-sm text-muted" style={{ marginTop: "var(--space-2)" }}>
          {rig.keep_originals
            ? `${rig.name || rig.id} keeps its device originals, so its whole history can be re-filed.`
            : `${rig.name || rig.id} deletes its device originals once they're verified, and Commander drops those rows a week later — so re-file reaches roughly the last week plus everything still on the device. Anything older stays where it is. Two devices running the same migration will legitimately see different counts.`}
        </p>
      )}

      {previewError && (
        <p className="notice tone-problem" role="alert" style={{ marginTop: "var(--space-3)" }}>
          <Icon name="problem" size={18} />
          <span>{previewError}</span>
        </p>
      )}

      {preview && (
        <div
          className="stack"
          style={{ ["--stack-gap" as string]: "var(--space-4)", marginTop: "var(--space-4)" }}
        >
          {stranded.length > 0 && (
            <p className="notice tone-working">
              <Icon name="alert" size={18} />
              <span className="notice__body">
                <strong>
                  {stranded.length} folder{stranded.length === 1 ? "" : "s"} probably
                  won't be removed once emptied.
                </strong>
                <span>
                  Each holds something Commander doesn't manage — a .DS_Store the
                  Finder wrote, or your own stacked masters. The frames still move;
                  the folder stays behind with whatever is left in it.
                </span>
                {stranded.slice(0, MAX_LISTED_DIRS).map((s) => (
                  <span key={s.dir} className="mono text-xs break-anywhere">
                    {s.dir} — {s.reason}
                  </span>
                ))}
                {stranded.length > MAX_LISTED_DIRS && (
                  <span className="text-xs">
                    …and {stranded.length - MAX_LISTED_DIRS} more.
                  </span>
                )}
              </span>
            </p>
          )}

          {refusals.length > 0 && (
            <p className="notice tone-neutral">
              <Icon name="info" size={18} />
              <span className="notice__body">
                <strong>
                  {refusals.length} file{refusals.length === 1 ? "" : "s"} can't be
                  moved and {refusals.length === 1 ? "is" : "are"} left alone.
                </strong>
                {refusals.slice(0, MAX_LISTED_DIRS).map((r) => (
                  <span key={r.dest_path} className="mono text-xs break-anywhere">
                    {r.dest_path} — {r.reason}
                  </span>
                ))}
                {refusals.length > MAX_LISTED_DIRS && (
                  <span className="text-xs">
                    …and {refusals.length - MAX_LISTED_DIRS} more.
                  </span>
                )}
              </span>
            </p>
          )}

          {groups.length === 0 ? (
            <EmptyState
              icon="safe"
              title="Nothing to move"
              description="Every frame in this scope is already filed where the current templates would put it."
            />
          ) : (
            <>
              <div className="cluster-between">
                <span className="text-sm text-muted" aria-live="polite">
                  {selectedGroups.length} of {groups.length} folder move
                  {groups.length === 1 ? "" : "s"} selected —{" "}
                  {selectedFiles.toLocaleString()} file
                  {selectedFiles === 1 ? "" : "s"}
                </span>
                <div className="cluster">
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={applyBusy || selected.size === groups.length}
                    onClick={() => setSelected(new Set(groups.map(groupKey)))}
                  >
                    Select all
                  </Button>
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={applyBusy || selected.size === 0}
                    onClick={() => setSelected(new Set())}
                  >
                    Deselect all
                  </Button>
                </div>
              </div>

              <ul className="list-reset refile-groups">
                {groups.map((g) => {
                  const risks = g.strand_risk ?? [];
                  return (
                    <li key={groupKey(g)} className="refile-group">
                      <input
                        type="checkbox"
                        className="aac-checkbox"
                        checked={selected.has(groupKey(g))}
                        disabled={applyBusy}
                        onChange={() => toggleGroup(g)}
                        aria-label={`Select ${g.from_dir} to ${g.to_dir}`}
                      />
                      <div className="refile-group__body">
                        <div className="refile-group__paths mono text-sm break-anywhere">
                          <span>{g.from_dir}</span>
                          <Icon name="chevron-right" size={14} />
                          <span>{g.to_dir}</span>
                        </div>
                        <p className="text-xs text-muted">
                          {g.count.toLocaleString()} file{g.count === 1 ? "" : "s"}
                        </p>
                        {risks.length > 0 && (
                          <p className="text-xs" style={{ color: "var(--work-fg)" }}>
                            The old folder will likely stay: {risks[0]!.reason}
                            {risks.length > 1 ? ` (+${risks.length - 1} more)` : ""}
                          </p>
                        )}
                      </div>
                    </li>
                  );
                })}
              </ul>

              <div className="cluster">
                <Button
                  variant="primary"
                  icon={<Icon name="reclassify" size={16} />}
                  loading={applyBusy}
                  disabled={selectedGroups.length === 0 || applyBusy}
                  onClick={runApply}
                >
                  Move {selectedFiles.toLocaleString()} file
                  {selectedFiles === 1 ? "" : "s"}
                </Button>
                {applyBusy && (
                  <Button variant="secondary" onClick={() => abort.current?.abort()}>
                    Stop
                  </Button>
                )}
              </div>
            </>
          )}
        </div>
      )}

      {progress && (
        <div
          className="stack"
          style={{ ["--stack-gap" as string]: "var(--space-3)", marginTop: "var(--space-5)" }}
        >
          <h3 className="aac-field__label">Move progress</h3>
          <p className="section-sub" aria-live="polite">
            {applyBusy
              ? `Moving folder ${Math.min(progress.done + 1, progress.total)} of ${progress.total}${
                  progress.group ? ` — up to ${progress.group.count.toLocaleString()} files` : ""
                }… Each folder is moved in one go, so its files are reported together when it finishes.`
              : stopped
                ? `Stopped after ${progress.done} of ${progress.total} folder moves. Nothing is half-moved — preview again to see what's left.`
                : `Finished ${progress.done} of ${progress.total} folder moves. Preview again to see what's left.`}
          </p>
          <div className="cluster">
            <StatusPill tone="safe" icon="safe" label={`${counts.moved} moved`} />
            {counts.skipped > 0 && (
              <StatusPill tone="neutral" icon="clock" label={`${counts.skipped} skipped`} />
            )}
            {counts.failed > 0 && (
              <StatusPill tone="problem" icon="problem" label={`${counts.failed} failed`} />
            )}
          </div>

          {failures.length > 0 && (
            <ul className="list-reset refile-outcomes">
              {failures.map((r, i) => (
                <li key={`${r.frame_id}-${i}`} className="refile-outcome">
                  <StatusPill
                    tone={r.status === "failed" ? "problem" : "neutral"}
                    icon={r.status === "failed" ? "problem" : "clock"}
                    label={r.status === "failed" ? "Failed" : "Skipped"}
                  />
                  <span className="mono text-xs break-anywhere grow">
                    {basename(r.from || r.to || "") || `frame ${r.frame_id}`}
                  </span>
                  {r.reason && <span className="text-xs text-muted">{r.reason}</span>}
                </li>
              ))}
            </ul>
          )}

          {prunedDirs.length > 0 && (
            <p className="text-sm text-muted">
              Removed {prunedDirs.length} emptied folder
              {prunedDirs.length === 1 ? "" : "s"}.
            </p>
          )}
          {skippedDirs.length > 0 && (
            <p className="notice tone-neutral">
              <Icon name="info" size={18} />
              <span className="notice__body">
                <strong>
                  {skippedDirs.length} folder{skippedDirs.length === 1 ? "" : "s"} left
                  in place.
                </strong>
                {skippedDirs.slice(0, MAX_LISTED_DIRS).map((s) => (
                  <span key={s.dir} className="mono text-xs break-anywhere">
                    {s.dir} — {s.reason}
                  </span>
                ))}
                {skippedDirs.length > MAX_LISTED_DIRS && (
                  <span className="text-xs">
                    …and {skippedDirs.length - MAX_LISTED_DIRS} more.
                  </span>
                )}
              </span>
            </p>
          )}
        </div>
      )}
    </Card>
  );
}
