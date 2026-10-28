/**
 * MissingFrames — the per-rig panel of frames whose filed NAS copy went missing
 * outside Commander (design library-reconciliation §4.7, §13.3).
 *
 * The rows are split in two sections. The actionable one leads: frames whose
 * original is usually still on the device, restorable, and acknowledgeable where
 * the server allows. Below it, collapsed, the leftovers already gone from the
 * device: nothing waits on them, Restore helps only if a card still holding the
 * original comes back, and Forget stops tracking them. Each section has its own
 * search, folder filter and page, a folder stack whose counts describe whole
 * folders (they ignore the filter, as the group actions do), and a paged table.
 */
import { useCallback, useEffect, useState, type ReactNode } from "react";
import {
  api,
  ApiError,
  type AckFrameChallenge,
  type AckGroupChallenge,
  type ForgetChallenge,
  type ForgetScope,
  type Frame,
  type MissingFramesPage,
  type MissingGroup,
  type MissingSection,
} from "@/api";
import {
  Button,
  Dialog,
  Icon,
  Spinner,
  StatusPill,
  Table,
  TextInput,
  missingFrameView,
  useToast,
  type Column,
} from "@/design";
import { FrameThumb } from "./FrameThumb";
import { FrameLightbox } from "./FrameLightbox";
import { absoluteTime, relativeTime } from "../util/format";
import {
  ackAllCount,
  clampPage,
  emptyActionableText,
  forgetButtonLabel,
  forgetDoneText,
  forgetScopeText,
  forgetTitle,
  initialSectionFilter,
  pageCount,
  skippedSummary,
  withDir,
  withQuery,
  type SectionFilter,
} from "../util/missingPanel";

/** How long the search box waits after the last keystroke before it queries. */
const SEARCH_DEBOUNCE_MS = 250;

/** The age scopes the leftover section offers (design library-reconciliation §13.4). */
const FORGET_AGES = [7, 28];

/** The reconciliation-settle-window discriminating copy (design library-reconciliation §10.2/§10.7), shared
 * verbatim between the per-frame and per-directory acknowledge dialogs. */
function AckWarningCopy() {
  return (
    <>
      <p>
        Only do this if these files were <strong>deleted</strong>. If you moved
        or renamed the folder, cancel — Commander can't follow a move, and the
        originals on the device are the only remaining copies.
      </p>
      <p>
        Commander will remove the original from the device on its next pass
        instead of waiting out the reconcile-settle window. It still re-checks
        that the device copy matches what it archived, but it will not wait for
        further confirmation that the file is really gone. This cannot be undone.
      </p>
    </>
  );
}

/** Maps an acknowledge- or forget-confirm failure to its toast copy (design library-reconciliation §10.7). Pre-write
 * refusals (a bad/expired token, or a 409 reason) surface the server's own message;
 * an unclassified (likely 500) failure never claims nothing happened, since a batch
 * confirm can have partially applied. */
function confirmErrorMessage(err: unknown, plural: boolean, verb: "Acknowledge" | "Forget"): string {
  if (err instanceof ApiError) {
    if (err.code === "ACK_CONFIRMATION_INVALID" || err.code === "FORGET_CONFIRMATION_INVALID") {
      return `That confirmation expired — click ${verb} again.`;
    }
    if (err.code) return err.message;
    const done = verb === "Forget" ? "forgotten" : "acknowledged";
    return plural
      ? `Something went wrong — some frames may have been ${done}. The list has been refreshed.`
      : `Something went wrong — this frame may have been ${done}. The list has been refreshed.`;
  }
  // A lost response (network drop) can't distinguish success from failure — the
  // request may have applied on the server, so don't assert it didn't. reloadOnError
  // has already refreshed the list to show the truth.
  return "The request may not have completed — the list has been refreshed.";
}

/** A confirmation token is dead once refused as invalid, so its dialog closes;
 * every other refusal leaves the dialog open so the operator sees why. */
function isDeadToken(err: unknown): boolean {
  return (
    err instanceof ApiError &&
    (err.code === "ACK_CONFIRMATION_INVALID" || err.code === "FORGET_CONFIRMATION_INVALID")
  );
}

interface FrameAckState {
  frame: Frame;
  challenge: AckFrameChallenge;
}

interface GroupAckState {
  dir: string;
  challenge: AckGroupChallenge;
}

interface ForgetState {
  scope: ForgetScope;
  challenge: ForgetChallenge;
}

/** The busy key of a bulk Forget, so only the button that started it spins. */
const forgetKey = (scope: ForgetScope) => `forget:${scope.dir ?? ""}:${scope.older_than_days ?? 0}`;

/** One section's filter and its current page of rows. A reload keeps the rows on
 * screen until the answer arrives, and a newer request aborts an older one, so a
 * slow answer to an earlier search never overwrites a later one. */
function useMissingSection(rigId: string, section: MissingSection, enabled: boolean) {
  const [filter, setFilter] = useState<SectionFilter>(initialSectionFilter);
  const [data, setData] = useState<MissingFramesPage | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    if (!enabled) return;
    const ctl = new AbortController();
    api
      .missingFrames(rigId, { section, page: filter.page, dir: filter.dir, q: filter.q }, ctl.signal)
      .then((d) => {
        setData(d);
        setError(null);
        // An action can empty the last page; step back to the new last page.
        setFilter((f) => clampPage(f, d.total, d.page_size));
      })
      .catch((err: unknown) => {
        if (ctl.signal.aborted) return;
        setError(err instanceof ApiError ? err.message : "Couldn't load missing frames.");
      });
    return () => ctl.abort();
  }, [rigId, section, filter, nonce, enabled]);

  const reload = useCallback(() => setNonce((n) => n + 1), []);
  return { filter, setFilter, data, error, reload };
}

type SectionState = ReturnType<typeof useMissingSection>;

/**
 * The body shared by both sections: search box, active-folder chip, folder stack,
 * table and pager. `folderAction` renders a folder line's group button, if any.
 */
function SectionBody({
  name,
  caption,
  state,
  columns,
  folderAction,
  empty,
}: {
  /** Lowercase noun for labels: "missing frames" or "leftovers". */
  name: string;
  caption: string;
  state: SectionState;
  columns: Column<Frame>[];
  folderAction: (g: MissingGroup) => ReactNode;
  empty: ReactNode;
}) {
  const { filter, setFilter, data, error } = state;
  const [text, setText] = useState(filter.q);

  useEffect(() => {
    const t = setTimeout(() => setFilter((f) => withQuery(f, text.trim())), SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(t);
  }, [text, setFilter]);

  const groups = data?.groups ?? [];
  const total = data?.total ?? 0;
  const totalPages = pageCount(total, data?.page_size ?? 0);

  return (
    <div className="stack missing-section__body">
      <TextInput
        type="search"
        value={text}
        onChange={(e) => setText(e.target.value)}
        placeholder="Search file paths"
        aria-label={`Search ${name}`}
      />
      {filter.dir && (
        <div className="chip-row">
          <button
            type="button"
            className="chip missing-chip"
            aria-label={`Show every folder, not only ${filter.dir}`}
            onClick={() => setFilter((f) => withDir(f, ""))}
          >
            <span className="break-anywhere">{filter.dir}</span>
            <Icon name="close" size={14} />
          </button>
        </div>
      )}
      {groups.length > 0 && (
        <ul className="missing-folders" aria-label={`Folders with ${name}`}>
          {groups.map((g) => (
            <li key={g.dir} className="missing-folder">
              <button
                type="button"
                className="missing-folder__name"
                aria-pressed={filter.dir === g.dir}
                onClick={() => setFilter((f) => withDir(f, g.dir))}
              >
                <span className="mono break-anywhere">{g.dir}</span> · {g.count}
              </button>
              {folderAction(g)}
            </li>
          ))}
        </ul>
      )}
      {error ? (
        <p className="notice tone-problem" role="alert">
          <Icon name="problem" size={18} />
          <span>{error}</span>
        </p>
      ) : !data ? (
        <Spinner label="Loading missing frames…" />
      ) : (
        <>
          <Table
            caption={caption}
            columns={columns}
            rows={data.frames ?? []}
            getKey={(f) => f.id}
            empty={empty}
          />
          {total > 0 && (
            <nav className="pager" aria-label={`Pages of ${name}`}>
              <Button
                size="sm"
                variant="secondary"
                disabled={filter.page <= 1}
                onClick={() => setFilter((f) => ({ ...f, page: Math.max(1, f.page - 1) }))}
              >
                Previous
              </Button>
              <span className="pager__status" aria-live="polite">
                {total} {name} · page {filter.page} of {totalPages}
              </span>
              <Button
                size="sm"
                variant="secondary"
                disabled={filter.page >= totalPages}
                onClick={() => setFilter((f) => ({ ...f, page: Math.min(totalPages, f.page + 1) }))}
              >
                Next
              </Button>
            </nav>
          )}
        </>
      )}
    </div>
  );
}

/**
 * Restore and Acknowledge act on the actionable section; Restore and Forget on the
 * leftovers. Acknowledge (per-frame and per-directory) waives the reconcile-settle
 * window for frames the operator culled on purpose (design library-reconciliation
 * §10). It is inert on a keep_originals rig, so no control is rendered there.
 * Library linking is global and lives on the Settings page, not here — the marker
 * keys the destination root, not the rig.
 */
export function MissingFrames({
  rigId,
  keepOriginals,
  missingWarning,
}: {
  rigId: string;
  keepOriginals: boolean;
  /** The rig's live missing warning, if any: an empty actionable section then
   * points at the leftovers. */
  missingWarning?: string;
}) {
  const toast = useToast();
  const [leftoversOpen, setLeftoversOpen] = useState(false);
  const actionable = useMissingSection(rigId, "actionable", true);
  // Loaded only once opened: the summary's count comes from either answer's totals.
  const leftover = useMissingSection(rigId, "leftover", leftoversOpen);
  // The action in flight, keyed so only its button spins: `restore:${id}` |
  // `cancel:${id}` | `ackmint:${id}` | `ackconfirm:${id}` | `ackgroupmint:${dir}` |
  // `ackgroupconfirm:${dir}` | `forgetrow:${id}` | forgetKey(scope) |
  // `forgetconfirm`.
  const [busy, setBusy] = useState<string | null>(null);
  const [lightbox, setLightbox] = useState<Frame | null>(null);
  const [ackFrame, setAckFrame] = useState<FrameAckState | null>(null);
  const [ackGroup, setAckGroup] = useState<GroupAckState | null>(null);
  const [forget, setForget] = useState<ForgetState | null>(null);

  // Any action can move rows between sections or change both totals.
  const { reload: reloadActionable } = actionable;
  const { reload: reloadLeftover } = leftover;
  const reload = useCallback(() => {
    reloadActionable();
    reloadLeftover();
  }, [reloadActionable, reloadLeftover]);

  const act = useCallback(
    async <T,>(
      key: string,
      fn: () => Promise<T>,
      done: string | ((result: T) => string),
      opts?: { reloadOnError?: boolean; errorMessage?: (err: unknown) => string },
    ) => {
      setBusy(key);
      try {
        const result = await fn();
        toast.success(typeof done === "string" ? done : done(result));
        reload();
      } catch (err) {
        toast.error(
          opts?.errorMessage
            ? opts.errorMessage(err)
            : err instanceof ApiError
              ? err.message
              : "That action didn't go through.",
        );
        if (opts?.reloadOnError) reload();
      } finally {
        setBusy(null);
      }
    },
    [reload, toast],
  );

  const restore = (f: Frame) =>
    act(`restore:${f.id}`, () => api.restoreFrame(rigId, f.id), "Restore requested.");
  const cancelRestore = (f: Frame) =>
    act(`cancel:${f.id}`, () => api.cancelRestore(rigId, f.id), "Restore cancelled.");
  const forgetRow = (f: Frame) =>
    act(
      `forgetrow:${f.id}`,
      () => api.forgetFrame(rigId, f.id),
      (r) =>
        r?.code === "FORGET_QUEUED"
          ? "The rig is busy; this may already have applied."
          : "Frame forgotten.",
      { reloadOnError: true },
    );

  // Mint a per-frame confirmation. `{already: true}` means the frame is already
  // acknowledged — no dialog to show, just refresh so the pill reflects it.
  const startFrameAck = useCallback(
    async (f: Frame) => {
      setBusy(`ackmint:${f.id}`);
      try {
        const res = await api.acknowledgeFrameChallenge(rigId, f.id);
        if ("already" in res) {
          reload();
          return;
        }
        setAckFrame({ frame: f, challenge: res });
      } catch (err) {
        toast.error(
          err instanceof ApiError
            ? err.message
            : "Couldn't check whether this frame can be acknowledged.",
        );
      } finally {
        setBusy(null);
      }
    },
    [rigId, reload, toast],
  );

  // Mint a confirmation for a folder or section. A challenge with nothing to bind is
  // a 409 *_NOTHING_TO_DO whose message the server builds from its counts; the
  // folder lines it was clicked from are stale, so refresh them.
  const startChallenge = useCallback(
    async <T,>(key: string, mint: () => Promise<T>, open: (c: T) => void, fallback: string) => {
      setBusy(key);
      try {
        open(await mint());
      } catch (err) {
        toast.error(err instanceof ApiError ? err.message : fallback);
        if (
          err instanceof ApiError &&
          (err.code === "ACK_NOTHING_TO_DO" || err.code === "FORGET_NOTHING_TO_DO")
        ) {
          reload();
        }
      } finally {
        setBusy(null);
      }
    },
    [toast, reload],
  );

  const startGroupAck = (dir: string) =>
    startChallenge(
      `ackgroupmint:${dir}`,
      () => api.acknowledgeGroupChallenge(rigId, dir),
      (challenge) => setAckGroup({ dir, challenge }),
      "Couldn't check whether this directory can be acknowledged.",
    );

  const startForget = (scope: ForgetScope) =>
    startChallenge(
      forgetKey(scope),
      () => api.forgetLeftoversChallenge(rigId, scope),
      (challenge) => setForget({ scope, challenge }),
      "Couldn't check what can be forgotten.",
    );

  // Confirm closes its dialog on success and on an expired/invalid token (design
  // library-reconciliation §10.7: that token is dead, so leaving the dialog open
  // just invites a repeat 403) but stays open on every other refusal so the
  // operator sees why. reloadOnError is set unconditionally: a 500 from a batched
  // confirm can have partially applied, and silently not refreshing would repeat
  // the untruthful-outcome hazard the whole confirm handshake exists to avoid.
  const confirmWith = <T,>(confirm: () => Promise<T>, close: () => void) => async () => {
    try {
      const r = await confirm();
      close();
      return r;
    } catch (err) {
      if (isDeadToken(err)) close();
      throw err;
    }
  };

  const confirmFrameAck = () => {
    if (!ackFrame) return;
    const { frame, challenge } = ackFrame;
    void act(
      `ackconfirm:${frame.id}`,
      confirmWith(() => api.acknowledgeFrameConfirm(rigId, frame.id, challenge.token), () => setAckFrame(null)),
      (r) =>
        r?.code === "ACK_QUEUED"
          ? "The rig is busy; this may already have applied."
          : "Frame acknowledged.",
      { reloadOnError: true, errorMessage: (err) => confirmErrorMessage(err, false, "Acknowledge") },
    );
  };

  const confirmGroupAck = () => {
    if (!ackGroup) return;
    const { dir, challenge } = ackGroup;
    void act(
      `ackgroupconfirm:${dir}`,
      confirmWith(() => api.acknowledgeGroupConfirm(rigId, dir, challenge.token), () => setAckGroup(null)),
      (r) =>
        r?.code === "ACK_QUEUED"
          ? "The rig is busy; this may already have applied."
          : `Acknowledged ${r?.acknowledged ?? 0} frame${r?.acknowledged === 1 ? "" : "s"}.`,
      { reloadOnError: true, errorMessage: (err) => confirmErrorMessage(err, true, "Acknowledge") },
    );
  };

  const confirmForget = () => {
    if (!forget) return;
    const { scope, challenge } = forget;
    void act(
      "forgetconfirm",
      confirmWith(() => api.forgetLeftoversConfirm(rigId, scope, challenge.token), () => setForget(null)),
      forgetDoneText,
      { reloadOnError: true, errorMessage: (err) => confirmErrorMessage(err, true, "Forget") },
    );
  };

  const baseColumns: Column<Frame>[] = [
    {
      key: "thumb",
      header: <span className="sr-only">Preview</span>,
      cell: (f) => <FrameThumb frame={f} onOpen={setLightbox} />,
    },
    {
      key: "file",
      header: "Filed copy (on the NAS)",
      cell: (f) => <span className="mono">{f.dest_path || f.source_path}</span>,
      width: 260,
    },
    {
      key: "type",
      header: "Type",
      cell: (f) => <span className="mono">{f.frame_type || "—"}</span>,
    },
    {
      key: "missing",
      header: "Missing since",
      cell: (f) =>
        f.missing_seen_at ? (
          <span title={absoluteTime(f.missing_seen_at)}>
            {relativeTime(f.missing_seen_at)}
          </span>
        ) : (
          <>—</>
        ),
      numeric: true,
    },
    {
      key: "status",
      header: "Status",
      cell: (f) => (
        <StatusPill
          view={missingFrameView({
            restorePending: !!f.restore_requested_at,
            acked:
              (f.state === "verified" || f.state === "unclassified") &&
              !!f.missing_seen_at &&
              !!f.missing_acked_at,
          })}
        />
      ),
    },
  ];

  const restoreButton = (f: Frame) =>
    f.restore_requested_at ? (
      <Button
        size="sm"
        variant="ghost"
        loading={busy === `cancel:${f.id}`}
        onClick={() => cancelRestore(f)}
      >
        Cancel
      </Button>
    ) : (
      <Button
        size="sm"
        variant="secondary"
        loading={busy === `restore:${f.id}`}
        onClick={() => restore(f)}
      >
        Restore
      </Button>
    );

  const actionColumn = (cell: (f: Frame) => ReactNode): Column<Frame> => ({
    key: "action",
    header: <span className="sr-only">Frame actions</span>,
    cell: (f) => (
      <div className="cluster" style={{ ["--cluster-gap" as string]: "var(--space-2)" }}>
        {restoreButton(f)}
        {cell(f)}
      </div>
    ),
  });

  const actionableColumns = [
    ...baseColumns,
    actionColumn(
      (f) =>
        !keepOriginals &&
        (f.state === "verified" || f.state === "unclassified") &&
        f.missing_seen_at &&
        !f.restore_requested_at &&
        !f.missing_acked_at && (
          <Button
            size="sm"
            variant="secondary"
            loading={busy === `ackmint:${f.id}`}
            onClick={() => startFrameAck(f)}
          >
            Acknowledge
          </Button>
        ),
    ),
  ];

  // A pending restore is the operator's other standing intent; the server refuses
  // to forget it silently, so the button waits for the restore to be cancelled.
  const leftoverColumns = [
    ...baseColumns,
    actionColumn((f) => (
      <Button
        size="sm"
        variant="ghost"
        disabled={!!f.restore_requested_at}
        title={f.restore_requested_at ? "Cancel the pending restore first" : undefined}
        loading={busy === `forgetrow:${f.id}`}
        onClick={() => forgetRow(f)}
      >
        Forget
      </Button>
    )),
  ];

  const ackButton = (g: MissingGroup) => {
    const n = ackAllCount(g, keepOriginals);
    return (
      n !== null && (
        <Button
          size="sm"
          variant="secondary"
          loading={busy === `ackgroupmint:${g.dir}`}
          onClick={() => startGroupAck(g.dir)}
        >
          Acknowledge all {n}
        </Button>
      )
    );
  };

  const forgetFolderButton = (g: MissingGroup) => (
    <Button
      size="sm"
      variant="secondary"
      loading={busy === forgetKey({ dir: g.dir })}
      onClick={() => startForget({ dir: g.dir })}
    >
      {forgetButtonLabel({ dir: g.dir }, g.count)}
    </Button>
  );

  const totals = actionable.data?.totals ?? leftover.data?.totals;
  const leftoverTotal = totals?.leftover ?? 0;
  const groupSkipped = ackGroup ? skippedSummary(ackGroup.challenge.skipped) : null;

  return (
    <>
      <p className="notice tone-info">
        <Icon name="info" size={18} />
        <span className="notice__body">
          <span>
            <strong>Missing</strong> frames had their filed copy removed from the
            NAS outside Commander — so Commander treats them as deliberately culled.
          </span>
          <span>
            Once a frame has been missing for the reconcile-settle window, its
            original is removed from the device too. <strong>Restore</strong> pulls
            the original back to the NAS instead, and holds it in the meantime.
          </span>
          <span>
            <strong>Acknowledge</strong> skips that wait for frames you culled on
            purpose.
          </span>
        </span>
      </p>

      <section className="stack missing-section" aria-labelledby={`missing-actionable-${rigId}`}>
        <h3 className="missing-section__title" id={`missing-actionable-${rigId}`}>
          Still on the device{totals ? ` (${totals.actionable})` : ""}
        </h3>
        <SectionBody
          name="missing frames"
          caption="Missing frames still on the device"
          state={actionable}
          columns={actionableColumns}
          folderAction={ackButton}
          empty={
            <p className="text-sm text-muted missing-empty">
              {actionable.filter.dir || actionable.filter.q
                ? "No missing frames match this filter."
                : emptyActionableText(!!missingWarning)}
            </p>
          }
        />
      </section>

      {(leftoverTotal > 0 || leftoversOpen) && (
        <details
          className="missing-section missing-leftovers"
          open={leftoversOpen}
          onToggle={(e) => setLeftoversOpen((e.currentTarget as HTMLDetailsElement).open)}
        >
          <summary className="missing-section__title">
            Already gone from the device ({leftoverTotal})
          </summary>
          <div className="stack">
            <p className="text-sm text-muted">
              These frames were removed from both the device and the NAS, so
              nothing is waiting on them. <strong>Restore</strong> works only if a
              card still holding the original is reinserted; <strong>Forget</strong>{" "}
              stops tracking them.
            </p>
            <div className="cluster">
              {[undefined, ...FORGET_AGES].map((days) => {
                const scope: ForgetScope = days ? { older_than_days: days } : {};
                return (
                  <Button
                    key={days ?? 0}
                    size="sm"
                    variant="secondary"
                    disabled={leftoverTotal === 0}
                    loading={busy === forgetKey(scope)}
                    onClick={() => startForget(scope)}
                  >
                    {forgetButtonLabel(scope)}
                  </Button>
                );
              })}
            </div>
            <SectionBody
              name="leftovers"
              caption="Missing frames already gone from the device"
              state={leftover}
              columns={leftoverColumns}
              folderAction={forgetFolderButton}
              empty={
                <p className="text-sm text-muted missing-empty">
                  {leftover.filter.dir || leftover.filter.q
                    ? "No leftovers match this filter."
                    : "Nothing left to forget."}
                </p>
              }
            />
          </div>
        </details>
      )}

      <FrameLightbox frame={lightbox} onClose={() => setLightbox(null)} />

      <Dialog
        open={!!ackFrame}
        onClose={() => setAckFrame(null)}
        title="Delete this frame from the device?"
        footer={
          <>
            <Button variant="ghost" onClick={() => setAckFrame(null)}>
              Keep waiting
            </Button>
            <Button
              variant="danger"
              loading={!!ackFrame && busy === `ackconfirm:${ackFrame.frame.id}`}
              onClick={confirmFrameAck}
            >
              Delete from device
            </Button>
          </>
        }
      >
        {ackFrame && (
          <>
            <p>
              Filed copy: <span className="mono">{ackFrame.challenge.dest_path}</span>
            </p>
            <p>
              Device original:{" "}
              <span className="mono">{ackFrame.challenge.source_path}</span>
            </p>
            <AckWarningCopy />
          </>
        )}
      </Dialog>

      <Dialog
        open={!!ackGroup}
        onClose={() => setAckGroup(null)}
        title={
          ackGroup
            ? `Delete ${ackGroup.challenge.count} frame${ackGroup.challenge.count === 1 ? "" : "s"} from the device?`
            : "Delete frames from the device?"
        }
        footer={
          <>
            <Button variant="ghost" onClick={() => setAckGroup(null)}>
              Keep waiting
            </Button>
            <Button
              variant="danger"
              loading={!!ackGroup && busy === `ackgroupconfirm:${ackGroup.dir}`}
              onClick={confirmGroupAck}
            >
              Delete from device
            </Button>
          </>
        }
      >
        {ackGroup && (
          <>
            {ackGroup.challenge.dir_gone && (
              <p className="notice tone-problem" role="alert">
                <Icon name="problem" size={18} />
                <span>
                  The folder itself is gone, not just its files. If you moved or
                  renamed it rather than deleting it, cancel.
                </span>
              </p>
            )}
            <p>
              Directory: <span className="mono">{ackGroup.dir}</span> ·{" "}
              {ackGroup.challenge.count} frame{ackGroup.challenge.count === 1 ? "" : "s"}
            </p>
            {groupSkipped && (
              <p className="text-sm text-muted">Not included: {groupSkipped}.</p>
            )}
            <AckWarningCopy />
          </>
        )}
      </Dialog>

      <Dialog
        open={!!forget}
        onClose={() => setForget(null)}
        title={forget ? forgetTitle(forget.challenge.count) : "Forget frames?"}
        footer={
          <>
            <Button variant="ghost" onClick={() => setForget(null)}>
              Keep tracking
            </Button>
            <Button
              variant="primary"
              loading={busy === "forgetconfirm"}
              onClick={confirmForget}
            >
              Forget
            </Button>
          </>
        }
      >
        {forget && (
          <>
            {forget.scope.dir && (
              <p>
                Folder: <span className="mono break-anywhere">{forget.scope.dir}</span>
              </p>
            )}
            <p>{forgetScopeText(forget.scope)}</p>
            {forget.challenge.skipped.restore_pending > 0 && (
              <p className="text-sm text-muted">
                Not included: {forget.challenge.skipped.restore_pending} with a pending
                restore. Cancel the restore first to forget those.
              </p>
            )}
            <p>
              Commander stops tracking these frames. No file is deleted, on the NAS
              or the device. If a card still holding one of these originals is
              reinserted, or the device is listing one right now, Commander pulls it
              back to the NAS on its next pass.
            </p>
          </>
        )}
      </Dialog>
    </>
  );
}
