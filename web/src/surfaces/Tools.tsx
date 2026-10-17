/**
 * Tools — a page for operator utilities on already-archived frames. The first
 * (and so far only) tool is frame retargeting (design
 * routing-and-filing §9.7): fix a frame whose ASIAIR Autorun target
 * name was wrong at capture time by re-labelling its FITS OBJECT card and
 * re-filing it to the correct target folder.
 *
 * The frame list reuses the existing cross-rig transfer-history read path
 * (api.listTransferHistory) rather than a new backend list endpoint — the
 * backend doesn't yet support a target-substring or date_obs range filter
 * server-side (a later wave adds that), so the date range and target filters
 * here narrow the currently-loaded page client-side. Paths shown by Preview are
 * the intended destination; a real name collision at Apply time may disambiguate
 * the filename slightly.
 *
 * Selection may span rigs (the cross-rig list already mixes them); Preview and
 * Apply each group the selection by rig_id and issue one request per rig, since
 * both retarget endpoints are per-rig (`/api/rigs/{id}/retarget/...`).
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router";
import {
  api,
  applyRefile,
  applyRetarget,
  ApiError,
  previewRefile,
  previewRetarget,
  type Frame,
  type RefileGroup,
  type RefilePreview,
  type RefileResult,
  type RefileScope,
  type RefileStatus,
  type RetargetPreview,
  type RetargetResult,
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
  retargetResultView,
  useToast,
  type Column,
} from "@/design";
import { basename } from "../util/format";
import "./surfaces.css";
import "./tools.css";

const PAGE_SIZE = 200; // the backend's historyMaxPageSize clamp
const MAX_TARGET_LEN = 60; // matches the FITS OBJECT card's value budget (design routing-and-filing §9.1)

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

/** Non-empty ⇒ why the typed target can't be used yet. */
function validateTarget(raw: string): string | undefined {
  const t = raw.trim();
  if (!t) return "Enter the corrected target name.";
  if (t.length > MAX_TARGET_LEN) {
    return `Target names must be ${MAX_TARGET_LEN} characters or fewer (currently ${t.length}).`;
  }
  return undefined;
}

/**
 * The spellings already filed in a preview row's destination folder, as one
 * readable phrase. A folder can hold more than a handful, and the point is that
 * there is divergence at all — so three are named and the rest counted.
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

  // ---- new-target input, live-validated -----------------------------------
  const [newTarget, setNewTarget] = useState("");
  const [attempted, setAttempted] = useState(false);
  const targetError =
    attempted || newTarget.length > 0 ? validateTarget(newTarget) : undefined;
  const canAct = selectedFrames.length > 0 && !validateTarget(newTarget);

  // ---- preview -------------------------------------------------------------
  const [previewBusy, setPreviewBusy] = useState(false);
  const [previews, setPreviews] = useState<RetargetPreview[] | null>(null);
  const [previewFrames, setPreviewFrames] = useState<Frame[]>([]);
  const [previewTarget, setPreviewTarget] = useState("");

  const runPreview = useCallback(async () => {
    setAttempted(true);
    if (selectedFrames.length === 0 || validateTarget(newTarget) || previewBusy) {
      return;
    }
    const target = newTarget.trim();
    setPreviewBusy(true);
    const entries = Array.from(groupByRig(selectedFrames).entries());
    const settled = await Promise.allSettled(
      entries.map(([rigId, frames]) =>
        previewRetarget(rigId, frames.map((f) => f.id), target),
      ),
    );
    const merged: RetargetPreview[] = [];
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
    setPreviewTarget(target);
    setPreviewBusy(false);
    if (failures === 0 && merged.length > 0) {
      const ineligible = merged.filter((p) => !p.eligible).length;
      toast.info(
        ineligible > 0
          ? `${merged.length - ineligible} of ${merged.length} frames can be retargeted; ${ineligible} ineligible.`
          : `All ${merged.length} frames can be retargeted.`,
      );
    }
  }, [newTarget, selectedFrames, previewBusy, toast, rigName]);

  const previewFrameById = useMemo(() => {
    const m = new Map<number, Frame>();
    for (const f of previewFrames) m.set(f.id, f);
    return m;
  }, [previewFrames]);

  /** How many of the selected frames Apply would actually retarget, or null when
   * that is unknown — no preview yet, a preview whose per-rig requests didn't all
   * answer, or one taken against a different selection or target. The confirmation
   * states blast radius, so it may only quote a count the preview still covers. */
  const eligibleCount = useMemo(() => {
    if (!previews || previewTarget !== newTarget.trim()) return null;
    if (previews.length !== selectedFrames.length) return null;
    const covered = new Set(previews.map((p) => p.frame_id));
    if (!selectedFrames.every((f) => covered.has(f.id))) return null;
    return previews.filter((p) => p.eligible).length;
  }, [previews, previewTarget, newTarget, selectedFrames]);

  // Without a covering preview the confirmation quotes the selection as a ceiling
  // ("up to N"), never as a promise, because the skipped share is unknown.
  const confirmCount = eligibleCount ?? selectedFrames.length;
  const confirmSkipped = eligibleCount === null ? 0 : selectedFrames.length - eligibleCount;
  const confirmDetail =
    eligibleCount === null
      ? "Frames already carrying that target, or otherwise ineligible, are skipped — preview to see the exact count."
      : confirmSkipped > 0
        ? `The other ${confirmSkipped} selected frame${confirmSkipped === 1 ? " is" : "s are"} skipped — already on that target, or otherwise ineligible.`
        : null;

  // ---- apply (streamed, per-rig, live progress) ----------------------------
  const [applyConfirmOpen, setApplyConfirmOpen] = useState(false);
  const [applyBusy, setApplyBusy] = useState(false);
  const [applyFrames, setApplyFrames] = useState<Frame[]>([]);
  const [applyResults, setApplyResults] = useState<Map<number, RetargetResult>>(new Map());

  const openApplyConfirm = () => {
    setAttempted(true);
    if (!canAct) return;
    setApplyConfirmOpen(true);
  };

  const runApply = useCallback(async () => {
    const target = newTarget.trim();
    const frames = selectedFrames;
    setApplyConfirmOpen(false);
    if (frames.length === 0) return;

    setApplyBusy(true);
    setApplyFrames(frames);
    setApplyResults(new Map());

    const localResults = new Map<number, RetargetResult>();
    const entries = Array.from(groupByRig(frames).entries());
    const settled = await Promise.allSettled(
      entries.map(([rigId, rigFrames]) =>
        applyRetarget(rigId, rigFrames.map((f) => f.id), target, (r) => {
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
          `Retarget failed — ${rigName(rigId)}`,
        );
      }
    });

    setApplyBusy(false);
    setPreviews(null);
    reload(); // filed frames now carry a new target/dest_path

    let retargeted = 0;
    let skipped = 0;
    let failed = 0;
    for (const r of localResults.values()) {
      if (r.status === "retargeted") retargeted++;
      else if (r.status === "skipped") skipped++;
      else failed++;
    }
    if (rigFailures === 0) {
      toast.success(
        `Retargeted ${retargeted} of ${frames.length} to "${target}"` +
          (skipped || failed ? ` (${skipped} skipped, ${failed} failed).` : "."),
        skipped || failed ? undefined : "Retarget complete",
      );
    }
  }, [newTarget, selectedFrames, toast, rigName, reload]);

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
      cell: (f) => (f.exptime ? `${f.exptime}s` : "—"),
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
            Utilities for already-archived frames. Retarget re-labels frames captured
            under the wrong Autorun target — it rewrites the FITS header and refiles
            the frame. Re-file moves frames that are already labelled correctly to
            where your current folder templates say they belong. Device originals are
            never touched by either.
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
              caption="Frames available for retargeting"
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
        <h2 className="section-title">Retarget</h2>
        <div className="form-grid" style={{ marginTop: "var(--space-3)" }}>
          <Field
            label="New target"
            help={`Replaces the FITS OBJECT header and refiles the frame under this name. Up to ${MAX_TARGET_LEN} characters.`}
            example="'NGC 7000' → 'NGC7000'"
            error={targetError}
            required
          >
            {(f) => (
              <TextInput
                {...f}
                value={newTarget}
                onChange={(e) => setNewTarget(e.target.value)}
                placeholder="e.g. NGC7000"
                disabled={applyBusy}
                invalid={!!targetError}
              />
            )}
          </Field>
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
          <ul className="list-reset retarget-preview">
            {previews.map((p) => {
              const f = previewFrameById.get(p.frame_id);
              return (
                <li
                  key={p.frame_id}
                  className={`retarget-preview__item${
                    p.eligible ? "" : " retarget-preview__item--ineligible"
                  }`}
                >
                  <div className="retarget-preview__head">
                    <span className="text-sm text-muted">
                      {f ? rigName(f.rig_id) : ""} — {p.current_target || "(no target)"}
                    </span>
                    {!p.eligible && <StatusPill tone="problem" icon="problem" label="Ineligible" />}
                  </div>
                  <div className="retarget-preview__paths mono text-sm break-anywhere">
                    <span>{p.old_path}</span>
                    <Icon name="chevron-right" size={14} />
                    <span>{p.new_path}</span>
                  </div>
                  {p.night_approx && (
                    <p className="text-xs text-muted">
                      Night number may change — another frame filed before you apply can
                      shift it.
                    </p>
                  )}
                  {p.existing_spellings && p.existing_spellings.length > 0 && (
                    <p className="text-xs text-muted">
                      Already filed here as {spellingList(p.existing_spellings)} — applying
                      will leave two spellings of this target in one folder.
                    </p>
                  )}
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
              ? `Retargeting ${applyResults.size} of ${applyFrames.length}…`
              : `Done — ${applyFrames.length} frame${applyFrames.length === 1 ? "" : "s"} processed.`}
          </p>
          <ul className="list-reset retarget-progress">
            {applyFrames.map((f) => {
              const r = applyResults.get(f.id);
              return (
                <li key={f.id} className="retarget-progress__item">
                  <span className="mono text-sm break-anywhere grow">
                    {basename(f.dest_path || f.source_path)}
                  </span>
                  {r ? (
                    <>
                      <StatusPill view={retargetResultView(r.status)} />
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
        title="Retarget frames?"
        footer={
          <>
            <Button variant="ghost" onClick={() => setApplyConfirmOpen(false)}>
              Cancel
            </Button>
            <Button variant="primary" onClick={runApply}>
              Retarget {eligibleCount === null && "up to "}
              {confirmCount} frame{confirmCount === 1 ? "" : "s"}
            </Button>
          </>
        }
      >
        <p>
          This rewrites the FITS <span className="mono">OBJECT</span> header and
          refiles {eligibleCount === null && "up to "}
          <strong>{confirmCount}</strong> frame{confirmCount === 1 ? "" : "s"} to{" "}
          <strong>{newTarget.trim()}</strong>. {confirmDetail} Device originals are
          never touched, and each frame's outcome is reported as it completes.
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
