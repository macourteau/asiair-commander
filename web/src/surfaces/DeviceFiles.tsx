/**
 * File browser — the page (/files) for the files on the ASIAIRs themselves
 * (design device-file-browser §3, §3.1 and §6): two panes,
 * each on a managed rig, with download, copy between panes, copy to the same
 * location on the other rig, upload from this computer, new folder, rename and
 * delete.
 *
 * Every rule a test needs (the toolbar's enable rules, the copy plan's decisions,
 * the report wording, the client-built report of a stopped run) lives in
 * util/deviceFiles.ts; this file wires them to the API and the design system.
 *
 * Copy, upload and delete runs stream NDJSON on the request, so they last as long
 * as the page: leaving it, or pressing Stop, aborts the fetch and the server
 * cancels the run. A stream that ends without its summary line is reported by
 * the client. An upload first sends the file to Commander, which keeps it until
 * the dialog closes, so its runs and retries write from there.
 */
import {
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useBlocker, useSearchParams } from "react-router";
import {
  api,
  ApiError,
  checkDeviceDownload,
  deviceDownloadUrl,
  discardUpload,
  listDeviceFiles,
  makeDeviceFolder,
  planDeviceCopy,
  planDeviceDelete,
  planDeviceUpload,
  renameDeviceEntry,
  runDeviceCopy,
  runDeviceDelete,
  runDeviceUpload,
  stageUpload,
  useSnapshot,
  type CopyOverwrite,
  type CopyPlan,
  type DeletePlan,
  type DeviceFileEntry,
  type DeviceFileListing,
  type FilesItemLine,
  type FilesProgressLine,
  type FilesRunLine,
  type FilesSummaryLine,
  type RigProfile,
  type RigStatus,
  type UploadPlan,
} from "@/api";
import {
  Badge,
  Button,
  Card,
  Dialog,
  EmptyState,
  Field,
  Icon,
  IconButton,
  Select,
  Spinner,
  StatusPill,
  Table,
  TextInput,
  useToast,
  type Column,
} from "@/design";
import { absoluteTime, formatBytes } from "../util/format";
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
  initialDecisions,
  isListed,
  isUnknownRig,
  joinPath,
  leaveRunningText,
  listingRegionLabel,
  mergeRunReport,
  nonJsonUploadRefusal,
  overwriteConfirmText,
  overwriteNeedsConfirm,
  paneRigNotice,
  pathSegments,
  placeText,
  planFailedText,
  PROGRESS_LABEL,
  readPanePlace,
  reasonText,
  renameDoneText,
  renameErrorText,
  renameSelection,
  RENAME_UNCHANGED,
  reportCounts,
  rigNamedIn,
  rigOptions,
  rowOutcomeLabel,
  rowRunAnnouncement,
  RUNNING_ROW_LABEL,
  selectionText,
  sendFailedText,
  sendingNote,
  sendingText,
  settledRows,
  toolbarState,
  unarchivedCheckboxLabel,
  unarchivedGrewNotice,
  unarchivedNotice,
  sendStoppedText,
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
  waitingText,
  WATCHED_RENAME_NOTE,
  watchedFolderNotice,
  watchedRootLine,
  writePanePlace,
  WAITING_AFTER_MS,
  type ButtonState,
  type Decision,
  type DecisionAction,
  type Decisions,
  type PanePlace,
  type PaneToolbar,
  type ReportCounts,
  type ReportRow,
  type RowAction,
  type RunEnd,
  type RunOp,
  type Side,
  type ToolbarPane,
} from "../util/deviceFiles";
import "./surfaces.css";
import "./tools.css";

const SIDE_LABEL: Record<Side, string> = { left: "Left", right: "Right" };
const NO_PLACE: PanePlace = { rig: null, share: null, dir: "" };
/** A dead rig takes about 16 s to fail a listing, so say so well before that. */
const SLOW_LISTING_MS = 3000;

/** Codes that can come from either side of a cross-rig copy; their server
 * message names the rig. Every other code is about the destination. */
const DEVICE_FAULTS: ReadonlySet<string> = new Set([
  "unreachable",
  "denied",
  "in_use",
  "identity",
  "error",
]);

const isAbort = (err: unknown) => err instanceof DOMException && err.name === "AbortError";
const errText = (err: unknown) =>
  err instanceof ApiError ? err.message : "Couldn't reach Commander.";

/* ---- small hooks ---------------------------------------------------------- */

/** Whole seconds since `startedAt`, ticking while it is set. */
function useElapsed(startedAt: number | null): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (startedAt === null) return;
    setNow(Date.now());
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [startedAt]);
  return startedAt === null ? 0 : Math.max(0, Math.floor((now - startedAt) / 1000));
}

/** True once WAITING_AFTER_MS pass with no stream line; `seq` bumps per line. */
function useWaiting(active: boolean, seq: number): boolean {
  const [waiting, setWaiting] = useState(false);
  useEffect(() => {
    setWaiting(false);
    if (!active) return;
    const t = setTimeout(() => setWaiting(true), WAITING_AFTER_MS);
    return () => clearTimeout(t);
  }, [active, seq]);
  return waiting;
}

/** The pane's listing, its rig's share list and whether the rig accepts writes. */
interface PaneData {
  /** bad_path: the URL named a path the server refuses (".." or a marker, say). */
  status: "idle" | "loading" | "ok" | "error" | "not_found" | "bad_path";
  listing: DeviceFileListing | null;
  error: string | null;
  slow: boolean;
  shares: string[] | null;
  writable: boolean | null;
  retry: () => void;
}

function usePaneData(place: PanePlace, reloadToken: number): PaneData {
  const [state, setState] = useState<Omit<PaneData, "retry" | "shares" | "writable">>({
    status: "idle",
    listing: null,
    error: null,
    slow: false,
  });
  const [rigInfo, setRigInfo] = useState<{
    rig: string | null;
    shares: string[] | null;
    writable: boolean | null;
  }>({ rig: null, shares: null, writable: null });
  const [retryToken, setRetryToken] = useState(0);
  const retry = useCallback(() => setRetryToken((n) => n + 1), []);
  const { rig, share, dir } = place;

  // A new rig starts unknown: its shares and writability come from its own listings.
  useEffect(() => {
    setRigInfo({ rig, shares: null, writable: null });
  }, [rig]);

  useEffect(() => {
    if (!rig) {
      setState({ status: "idle", listing: null, error: null, slow: false });
      return;
    }
    const ctrl = new AbortController();
    setState({ status: "loading", listing: null, error: null, slow: false });
    const slow = setTimeout(() => setState((s) => ({ ...s, slow: true })), SLOW_LISTING_MS);
    listDeviceFiles(rig, share, dir, ctrl.signal)
      .then((listing) => {
        setState({ status: "ok", listing, error: null, slow: false });
        setRigInfo((info) =>
          info.rig !== rig
            ? info
            : {
                rig,
                writable: listing.writable,
                shares:
                  share === null
                    ? listing.entries.filter((e) => e.dir).map((e) => e.name)
                    : info.shares,
              },
        );
      })
      .catch((err) => {
        if (isAbort(err)) return;
        if (err instanceof ApiError && (err.code === "not_found" || err.code === "bad_path")) {
          // The server's not_found message says what is missing (a share, a
          // folder, or a file opened as one); a bad path has nothing to retry.
          setState({ status: err.code, listing: null, error: err.message, slow: false });
        } else {
          setState({ status: "error", listing: null, error: errText(err), slow: false });
        }
      })
      .finally(() => clearTimeout(slow));
    return () => {
      ctrl.abort();
      clearTimeout(slow);
    };
  }, [rig, share, dir, reloadToken, retryToken]);

  // Inside a share the pane still needs its rig's share list, for the other pane's
  // "Copy to same location" rule; fetched once per rig and per reload.
  const sharesFor = useRef<string>("");
  useEffect(() => {
    if (!rig || share === null) return;
    const key = `${rig}\u0000${reloadToken}`;
    if (sharesFor.current === key) return;
    sharesFor.current = key;
    const ctrl = new AbortController();
    listDeviceFiles(rig, null, "", ctrl.signal)
      .then((listing) =>
        setRigInfo((info) =>
          info.rig !== rig
            ? info
            : {
                rig,
                writable: listing.writable,
                shares: listing.entries.filter((e) => e.dir).map((e) => e.name),
              },
        ),
      )
      .catch((err) => {
        if (!isAbort(err)) sharesFor.current = "";
      });
    return () => ctrl.abort();
  }, [rig, share, reloadToken]);

  return { ...state, shares: rigInfo.shares, writable: rigInfo.writable, retry };
}

/* ---- the panel ------------------------------------------------------------ */

/** The File browser page: reads the live rig status and the configured rigs. */
export function DeviceFiles() {
  const snapshot = useSnapshot();
  const statuses = snapshot?.rigs ?? null;
  const [profiles, setProfiles] = useState<RigProfile[] | null>(null);
  // The configured rigs only tell an unconfigured rig from an unmanaged one, so
  // they are re-read when the snapshot's rig set changes, not on every snapshot.
  const rigSet = (statuses ?? []).map((s) => s.id).sort().join(",");
  useEffect(() => {
    let cancelled = false;
    api
      .listRigs()
      .then((list) => !cancelled && setProfiles(list))
      .catch(() => {
        /* the pane notices fall back to "offline"/no notice without it */
      });
    return () => {
      cancelled = true;
    };
  }, [rigSet]);
  return (
    <>
      <header className="surface-head">
        <div>
          <h1 className="surface-head__title">File browser</h1>
          <p className="surface-head__desc">
            Browse, download, upload, copy, rename and delete the files on your
            ASIAIRs: it works on the devices themselves, not on your library.
          </p>
        </div>
      </header>
      <DeviceFilesPanel statuses={statuses} profiles={profiles} />
    </>
  );
}

type OpenDialog =
  | { kind: "copy"; side: Side; mirror: boolean }
  | { kind: "upload"; side: Side; file: File }
  | { kind: "delete"; side: Side }
  | { kind: "mkdir"; side: Side }
  | { kind: "rename"; side: Side; name: string; dir: boolean }
  | null;

/** What a pane focuses next: its listing, its Rename or Upload button (the
 * listing when that is disabled), or a renamed entry once its re-listing shows
 * it (selected and scrolled into view). */
type FocusTarget =
  | { what: "listing" }
  | { what: "rename" }
  | { what: "upload" }
  | { what: "entry"; name: string };

interface DownloadState {
  side: Side;
  startedAt: number;
  ctrl: AbortController;
}

export function DeviceFilesPanel({
  statuses,
  profiles,
}: {
  /** The snapshot's rigs (managed only), or null before the first snapshot. */
  statuses: RigStatus[] | null;
  profiles: RigProfile[] | null;
}) {
  const [params, setParams] = useSearchParams();
  const places: Record<Side, PanePlace> = {
    left: readPanePlace(params, "left") ?? NO_PLACE,
    right: readPanePlace(params, "right") ?? NO_PLACE,
  };

  // Defaults apply only while the URL names neither pane's rig, and only once a
  // rig is online, so an empty fleet at load doesn't pin both panes to "none".
  useEffect(() => {
    if (!statuses) return;
    if (params.has("lrig") || params.has("rrig")) return;
    const [l, r] = defaultRigs(statuses);
    if (!l) return;
    setParams(
      (p) => {
        let next = writePanePlace(p, "left", { rig: l, share: null, dir: "" });
        next = writePanePlace(next, "right", { rig: r, share: null, dir: "" });
        return next;
      },
      { replace: true },
    );
  }, [statuses, params, setParams]);

  // A rig the URL names but Commander has never heard of is treated as no rig:
  // nothing to list, retry or wait on.
  const unknown: Record<Side, boolean> = {
    left: !!places.left.rig && isUnknownRig(places.left.rig, statuses ?? [], profiles),
    right: !!places.right.rig && isUnknownRig(places.right.rig, statuses ?? [], profiles),
  };
  const usable = (side: Side): PanePlace =>
    unknown[side] ? NO_PLACE : places[side];

  const [reloadToken, setReloadToken] = useState(0);
  const relist = useCallback(() => setReloadToken((n) => n + 1), []);
  const data: Record<Side, PaneData> = {
    left: usePaneData(usable("left"), reloadToken),
    right: usePaneData(usable("right"), reloadToken),
  };

  const [selected, setSelected] = useState<Record<Side, Set<string>>>({
    left: new Set(),
    right: new Set(),
  });
  const [focusTarget, setFocusTarget] = useState<Record<Side, FocusTarget | null>>({
    left: null,
    right: null,
  });
  const focusOn = useCallback(
    (side: Side, target: FocusTarget | null) => setFocusTarget((f) => ({ ...f, [side]: target })),
    [],
  );
  const setFocusSide = useCallback((side: Side) => focusOn(side, { what: "listing" }), [focusOn]);
  // The renamed entry, selected once its pane's re-listing shows it.
  const [renamed, setRenamed] = useState<{ side: Side; name: string } | null>(null);

  /** `focus` false keeps focus where it is, as on the rig selector. */
  const navigate = useCallback(
    (side: Side, place: PanePlace, focus = true) => {
      setParams((p) => writePanePlace(p, side, place), { replace: true });
      setSelected((s) => ({ ...s, [side]: new Set() }));
      setRenamed((r) => (r?.side === side ? null : r));
      if (focus) setFocusSide(side);
    },
    [setParams, setFocusSide],
  );

  // A re-list can drop selected rows (deleted, or gone from the device).
  useEffect(() => {
    setSelected((s) => {
      let changed = false;
      const next = { ...s };
      for (const side of ["left", "right"] as Side[]) {
        const names = new Set((data[side].listing?.entries ?? []).map((e) => e.name));
        const kept = new Set([...s[side]].filter((n) => names.has(n)));
        if (kept.size !== s[side].size) {
          next[side] = kept;
          changed = true;
        }
      }
      return changed ? next : s;
    });
  }, [data.left.listing, data.right.listing]); // eslint-disable-line react-hooks/exhaustive-deps

  const rigName = useCallback(
    (id: string | null) => {
      if (!id) return "";
      return (
        statuses?.find((s) => s.id === id)?.name ||
        profiles?.find((p) => p.id === id)?.name ||
        id
      );
    },
    [statuses, profiles],
  );

  const toolbarPane = (side: Side): ToolbarPane => {
    const place = usable(side);
    const entries = data[side].listing?.entries ?? [];
    return {
      rig: place.rig,
      rigName: rigName(place.rig),
      share: place.share,
      dir: place.dir,
      writable: data[side].writable,
      notFound: data[side].status === "not_found" || data[side].status === "bad_path",
      watched: !!data[side].listing?.watched,
      selected: entries
        .filter((e) => place.share !== null && selected[side].has(e.name))
        .map((e) => ({ name: e.name, dir: e.dir, renameRefused: e.rename_refused })),
      shares: data[side].shares,
    };
  };

  const [dialog, setDialog] = useState<OpenDialog>(null);
  const [running, setRunning] = useState(false);
  useEffect(() => {
    if (!renamed) return;
    const listing = data[renamed.side].listing;
    if (!listing?.entries.some((e) => e.name === renamed.name)) return;
    setSelected((s) => ({ ...s, [renamed.side]: new Set([renamed.name]) }));
    focusOn(renamed.side, { what: "entry", name: renamed.name });
    setRenamed(null);
  }, [renamed, data.left.listing, data.right.listing]); // eslint-disable-line react-hooks/exhaustive-deps
  const [download, setDownload] = useState<DownloadState | null>(null);
  const [paneError, setPaneError] = useState<Record<Side, string | null>>({
    left: null,
    right: null,
  });
  const busy = dialog !== null || download !== null;
  const toolbars = toolbarState(toolbarPane("left"), toolbarPane("right"), busy);

  // Leaving the page aborts a run's fetch, which stops the run: ask first.
  useEffect(() => {
    if (!running) return;
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [running]);
  const blocker = useBlocker(
    ({ currentLocation, nextLocation }) =>
      running && currentLocation.pathname !== nextLocation.pathname,
  );

  const toast = useToast();
  const startDownload = useCallback(
    async (side: Side) => {
      const place = places[side];
      const names = [...selected[side]];
      if (!place.rig || place.share === null || names.length === 0) return;
      const ctrl = new AbortController();
      setPaneError((e) => ({ ...e, [side]: null }));
      setDownload({ side, startedAt: Date.now(), ctrl });
      try {
        const check = await checkDeviceDownload(
          place.rig,
          place.share,
          place.dir,
          names,
          ctrl.signal,
        );
        // The check walked the selection, so failures land in the panel; the real
        // download is the browser's own, with its own progress.
        const a = document.createElement("a");
        a.href = deviceDownloadUrl(place.rig, place.share, place.dir, names);
        a.download = check.filename;
        a.style.display = "none";
        document.body.appendChild(a);
        a.click();
        a.remove();
        // The browser may show nothing until the first byte; say it started.
        toast.success(`Download started: ${check.filename}`);
      } catch (err) {
        if (!isAbort(err)) setPaneError((e) => ({ ...e, [side]: errText(err) }));
      } finally {
        setDownload(null);
        // The Cancel button that held focus is gone.
        setFocusSide(side);
      }
    },
    [places, selected, toast],
  );

  // Whatever closes a dialog, focus lands on its pane's listing: the toolbar
  // button that opened it was disabled while the dialog was up. Rename and
  // Upload are the exceptions: their button is usually enabled again (the
  // selection is unchanged; Upload needs none), so it takes focus back.
  const closeDialog = () => {
    if (dialog) {
      focusOn(
        dialog.side,
        dialog.kind === "rename" || dialog.kind === "upload" ? { what: dialog.kind } : { what: "listing" },
      );
    }
    setDialog(null);
    setRunning(false);
  };

  const otherOf = (side: Side): Side => (side === "left" ? "right" : "left");
  const statusOf = (id: string | null) => statuses?.find((s) => s.id === id);

  return (
    <Card pad style={{ marginBottom: "var(--space-5)" }}>
      <div className="dfb-panes">
        {(["left", "right"] as Side[]).map((side) => (
          <Pane
            key={side}
            side={side}
            place={places[side]}
            data={data[side]}
            statuses={statuses ?? []}
            profiles={profiles}
            rigName={rigName(places[side].rig)}
            unknownRig={unknown[side]}
            keepsOriginals={!!statusOf(places[side].rig)?.keep_originals}
            toolbar={toolbars[side]}
            selected={selected[side]}
            onSelect={(next) => setSelected((s) => ({ ...s, [side]: next }))}
            onNavigate={(place, focus) => navigate(side, place, focus)}
            focus={focusTarget[side]}
            onFocused={() => focusOn(side, null)}
            download={download?.side === side ? download : null}
            onDownload={() => startDownload(side)}
            error={paneError[side]}
            onAction={(kind) => {
              if (kind === "rename") {
                const entry = data[side].listing?.entries.find((e) => selected[side].has(e.name));
                if (entry) setDialog({ kind: "rename", side, name: entry.name, dir: entry.dir });
                return;
              }
              setDialog(
                kind === "copyOther"
                  ? { kind: "copy", side, mirror: false }
                  : kind === "copySame"
                    ? { kind: "copy", side, mirror: true }
                    : { kind, side },
              );
            }}
            onUpload={(file) => setDialog({ kind: "upload", side, file })}
          />
        ))}
      </div>

      {dialog?.kind === "copy" && (() => {
        const src = places[dialog.side];
        const dst = places[otherOf(dialog.side)];
        return (
          <CopyDialog
            source={{
              kind: "copy",
              from: {
                rig: src.rig!,
                rigName: rigName(src.rig),
                share: src.share!,
                dir: src.dir,
                names: [...selected[dialog.side]],
              },
              mirror: dialog.mirror,
            }}
            to={
              dialog.mirror
                ? { rig: dst.rig!, rigName: rigName(dst.rig), share: src.share!, dir: src.dir }
                : { rig: dst.rig!, rigName: rigName(dst.rig), share: dst.share!, dir: dst.dir }
            }
            onClose={closeDialog}
            onRunning={setRunning}
            onRelist={relist}
          />
        );
      })()}

      {dialog?.kind === "upload" && (() => {
        const place = places[dialog.side];
        return (
          <CopyDialog
            source={{ kind: "upload", file: dialog.file }}
            to={{ rig: place.rig!, rigName: rigName(place.rig), share: place.share!, dir: place.dir }}
            onClose={closeDialog}
            onRunning={setRunning}
            onRelist={relist}
          />
        );
      })()}

      {dialog?.kind === "rename" && (() => {
        const side = dialog.side;
        const place = places[side];
        return (
          <RenameDialog
            rig={place.rig!}
            rigName={rigName(place.rig)}
            share={place.share!}
            dir={place.dir}
            name={dialog.name}
            isDir={dialog.dir}
            onClose={closeDialog}
            onRelist={relist}
            onRenamed={(newName) => {
              setDialog(null);
              // Both panes re-list: the other may show the same folder, or one
              // inside the renamed folder, which then reads as gone.
              relist();
              setRenamed({ side, name: newName });
              toast.success(renameDoneText(dialog.name, newName));
            }}
          />
        );
      })()}

      {dialog?.kind === "delete" && (() => {
        const place = places[dialog.side];
        return (
          <DeleteDialog
            rig={place.rig!}
            rigName={rigName(place.rig)}
            address={statusOf(place.rig)?.address}
            share={place.share!}
            dir={place.dir}
            names={[...selected[dialog.side]]}
            onClose={closeDialog}
            onRunning={setRunning}
            onRelist={relist}
          />
        );
      })()}

      {dialog?.kind === "mkdir" && (() => {
        const side = dialog.side;
        const place = places[side];
        return (
          <NewFolderDialog
            rig={place.rig!}
            rigName={rigName(place.rig)}
            share={place.share!}
            dir={place.dir}
            onClose={closeDialog}
            onCreated={(name) => {
              setDialog(null);
              // The next step is usually copying into it, so open it.
              navigate(side, { ...place, dir: joinPath(place.dir, name) });
              relist();
            }}
          />
        );
      })()}

      <Dialog
        open={blocker.state === "blocked"}
        onClose={() => blocker.reset?.()}
        title="Stop the running operation?"
        footer={
          <>
            <Button variant="ghost" onClick={() => blocker.reset?.()}>
              Stay on this page
            </Button>
            <Button variant="danger" onClick={() => blocker.proceed?.()}>
              Leave and stop it
            </Button>
          </>
        }
      >
        <p>
          {leaveRunningText(
            dialog?.kind === "upload" || dialog?.kind === "copy" || dialog?.kind === "delete"
              ? dialog.kind
              : null,
          )}
        </p>
      </Dialog>
    </Card>
  );
}

/* ---- one pane ------------------------------------------------------------- */

type PaneAction = "copyOther" | "copySame" | "delete" | "mkdir" | "rename";

/** Folders first, then files, each by name ignoring case. */
function sortEntries(entries: DeviceFileEntry[]): DeviceFileEntry[] {
  return [...entries].sort(
    (a, b) =>
      Number(b.dir) - Number(a.dir) ||
      a.name.localeCompare(b.name, undefined, { sensitivity: "base" }),
  );
}

function Pane({
  side,
  place,
  data,
  statuses,
  profiles,
  rigName,
  unknownRig,
  keepsOriginals,
  toolbar,
  selected,
  onSelect,
  onNavigate,
  focus,
  onFocused,
  download,
  onDownload,
  error,
  onAction,
  onUpload,
}: {
  side: Side;
  place: PanePlace;
  data: PaneData;
  statuses: RigStatus[];
  profiles: RigProfile[] | null;
  rigName: string;
  /** The URL names a rig Commander does not know: shown as no rig. */
  unknownRig: boolean;
  keepsOriginals: boolean;
  toolbar: PaneToolbar;
  selected: Set<string>;
  onSelect: (next: Set<string>) => void;
  onNavigate: (place: PanePlace, focus?: boolean) => void;
  focus: FocusTarget | null;
  onFocused: () => void;
  download: DownloadState | null;
  onDownload: () => void;
  error: string | null;
  onAction: (kind: PaneAction) => void;
  /** A file picked for upload into this pane's folder. */
  onUpload: (file: File) => void;
}) {
  const id = useId();
  const listRef = useRef<HTMLDivElement>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (!focus) return;
    if (focus.what === "listing") listRef.current?.focus();
    else if (focus.what === "rename" || focus.what === "upload") {
      const button = document.getElementById(`${id}-${focus.what}`) as HTMLButtonElement | null;
      if (button && !button.disabled) button.focus();
      else listRef.current?.focus();
    } else {
      const box = document.getElementById(checkboxId(focus.name));
      box?.closest("tr")?.scrollIntoView?.({ block: "nearest" });
      box?.focus();
    }
    onFocused();
  }, [focus]); // eslint-disable-line react-hooks/exhaustive-deps

  // Said once per watched folder entered, not again on a re-list of it, and
  // cleared by the first selection there.
  const [folderNote, setFolderNote] = useState("");
  const notedFolder = useRef<string | null>(null);
  const watchedHere = place.share !== null && data.status === "ok" && !!data.listing?.watched;
  const folderKey = JSON.stringify([place.rig, place.share, place.dir]);
  useEffect(() => {
    notedFolder.current = null;
    setFolderNote("");
  }, [folderKey]);
  useEffect(() => {
    if (!watchedHere || notedFolder.current === folderKey) return;
    notedFolder.current = folderKey;
    setFolderNote(WATCHED_RENAME_NOTE);
  }, [folderKey, watchedHere]);
  useEffect(() => {
    if (selected.size > 0) setFolderNote("");
  }, [selected]);

  const options = rigOptions(statuses);
  // A configured rig that left the snapshot stays selected (with a notice), so it
  // needs an option to show as the select's value; an unknown id gets none.
  if (place.rig && !unknownRig && !options.some((o) => o.value === place.rig)) {
    options.push({ value: place.rig, label: rigName, disabled: true });
  }
  const notice = place.rig ? paneRigNotice(place.rig, statuses, profiles) : null;
  const inShare = place.share !== null;
  const entries = useMemo(() => sortEntries(data.listing?.entries ?? []), [data.listing]);
  const elapsed = useElapsed(download?.startedAt ?? null);

  const selectedEntries = entries.filter((e) => selected.has(e.name));
  const allSelected = entries.length > 0 && selectedEntries.length === entries.length;
  const someSelected = selectedEntries.length > 0 && !allSelected;

  const rowIndex = new Map(entries.map((e, i) => [e.name, i]));
  const checkboxId = (name: string) => `${id}-sel-${rowIndex.get(name)}`;

  const toggle = (name: string) => {
    const next = new Set(selected);
    if (next.has(name)) next.delete(name);
    else next.add(name);
    onSelect(next);
  };

  const columns: Column<DeviceFileEntry>[] = [];
  if (inShare) {
    columns.push({
      key: "sel",
      header: (
        <input
          type="checkbox"
          className="aac-checkbox"
          aria-label={`Select all (${rigName})`}
          checked={allSelected}
          disabled={entries.length === 0}
          ref={(el) => {
            if (el) el.indeterminate = someSelected;
          }}
          onChange={() => onSelect(allSelected ? new Set() : new Set(entries.map((e) => e.name)))}
        />
      ),
      cell: (e) => (
        <input
          id={checkboxId(e.name)}
          type="checkbox"
          className="aac-checkbox"
          aria-label={`Select ${e.name} (${rigName})`}
          checked={selected.has(e.name)}
          onChange={() => toggle(e.name)}
        />
      ),
      className: "dfb-col-fit",
      width: 40,
    });
  }
  columns.push(
    {
      key: "name",
      header: "Name",
      cell: (e) => (
        <div className="dfb-name">
          <Icon name={e.dir ? "folder" : "file"} size={16} />
          <div className="dfb-name__body">
            {e.dir ? (
              <button
                type="button"
                className="dfb-link break-anywhere"
                onClick={() =>
                  onNavigate(
                    inShare
                      ? { ...place, dir: joinPath(place.dir, e.name) }
                      : { ...place, share: e.name, dir: "" },
                  )
                }
              >
                {e.name}
              </button>
            ) : (
              // A file's name toggles its checkbox, as a label does: the target
              // most people aim for, with no extra stop in the tab order.
              <label htmlFor={checkboxId(e.name)} className="dfb-file break-anywhere">
                {e.name}
              </label>
            )}
            {e.watched && <Badge variant="accent">Watched</Badge>}
            {inShare && (
              <span className="dfb-name__meta mono text-xs text-muted">
                {absoluteTime(e.mtime)}
              </span>
            )}
          </div>
        </div>
      ),
    },
    {
      key: "size",
      header: "Size",
      numeric: true,
      className: "dfb-col-fit",
      cell: (e) => (e.dir ? "" : formatBytes(e.size)),
    },
    {
      key: "mtime",
      header: "Modified",
      className: "dfb-col-mtime dfb-col-fit",
      cell: (e) => <span className="mono text-sm">{absoluteTime(e.mtime)}</span>,
    },
  );

  const folderLabel = place.dir ? baseName(place.dir) : place.share ?? "";

  let listing: ReactNode;
  if (!place.rig || unknownRig) {
    listing = (
      <EmptyState
        icon="rig"
        title={unknownRig ? "Choose a rig" : "No rig"}
        description="Choose a rig above to browse its folders."
      />
    );
  } else if (data.status === "loading" || data.status === "idle") {
    listing = (
      <div className="dfb-loading">
        <Spinner label={data.slow ? `Still trying ${rigName}…` : "Loading…"} />
      </div>
    );
  } else if (data.status === "not_found" || data.status === "bad_path") {
    listing = (
      <div className="dfb-problem">
        <EmptyState
          icon="problem"
          title={
            data.status === "bad_path"
              ? "That path isn't valid"
              : data.error || `${folderLabel} no longer exists on ${rigName}`
          }
          description="Go up with the path above."
        />
      </div>
    );
  } else if (data.status === "error") {
    listing = (
      <div className="dfb-problem">
        <EmptyState
          icon="problem"
          title={`Couldn't list ${rigName}`}
          description={data.error}
          action={
            <Button variant="secondary" onClick={data.retry}>
              Try again
            </Button>
          }
        />
      </div>
    );
  } else {
    listing = (
      <Table
        caption={`${SIDE_LABEL[side]} pane: ${inShare ? folderLabel : `shares on ${rigName}`}`}
        columns={columns}
        rows={entries}
        getKey={(e) => e.name}
        className="dfb-table"
        empty={
          <p className="text-sm text-muted dfb-empty">
            {inShare ? "This folder is empty" : `${rigName} lists no shares`}
          </p>
        }
      />
    );
  }

  const button = (
    key: string,
    state: ButtonState,
    label: string,
    icon: ReactNode,
    onClick: () => void,
  ) => (
    <span className="dfb-tool" key={key}>
      <Button
        id={`${id}-${key}`}
        size="sm"
        variant="secondary"
        icon={icon}
        disabled={!state.enabled}
        title={state.reason}
        aria-describedby={state.reason ? `${id}-${key}-why` : undefined}
        onClick={onClick}
      >
        {label}
      </Button>
      {state.reason && (
        <span id={`${id}-${key}-why`} className="sr-only">
          {state.reason}
        </span>
      )}
    </span>
  );

  return (
    <section className="dfb-pane" aria-labelledby={`${id}-title`}>
      <h3 className="dfb-pane__title" id={`${id}-title`}>
        {SIDE_LABEL[side]}
      </h3>
      <Select
        aria-label={`${SIDE_LABEL[side]} pane rig`}
        value={place.rig ?? ""}
        onChange={(e) => onNavigate({ rig: e.target.value || null, share: null, dir: "" }, false)}
      >
        <option value="">— No rig —</option>
        {options.map((o) => (
          <option key={o.value} value={o.value} disabled={o.disabled}>
            {o.label}
          </option>
        ))}
      </Select>

      {/* The status container stays mounted so a notice appearing in it (a rig
          going offline) is announced. */}
      <div role="status" className="dfb-status">
        {notice && (
          <p className="notice tone-problem">
            <Icon name="problem" size={18} />
            <span>{notice}</span>
          </p>
        )}
      </div>

      {place.rig && !unknownRig && (
        <div className="dfb-crumbs">
          <nav aria-label={`${SIDE_LABEL[side]} pane location`}>
            <ol className="list-reset dfb-crumbs__list">
              <Crumb
                label={rigName}
                current={!inShare}
                onClick={() => onNavigate({ ...place, share: null, dir: "" })}
              />
              {inShare && (
                <Crumb
                  label={place.share!}
                  current={!place.dir}
                  onClick={() => onNavigate({ ...place, dir: "" })}
                />
              )}
              {inShare &&
                pathSegments(place.dir).map((seg, i, all) => (
                  <Crumb
                    key={seg.path}
                    label={seg.name}
                    current={i === all.length - 1}
                    onClick={() => onNavigate({ ...place, dir: seg.path })}
                  />
                ))}
            </ol>
          </nav>
          <IconButton
            label={`Refresh the ${SIDE_LABEL[side].toLowerCase()} pane`}
            icon={<Icon name="run-now" size={16} />}
            onClick={data.retry}
          />
        </div>
      )}

      {inShare && data.status === "ok" && data.listing?.watched && (
        <p className="notice tone-info">
          <Icon name="info" size={18} />
          <span>{watchedFolderNotice(rigName, keepsOriginals)}</span>
        </p>
      )}

      {place.rig && (
        <div className="dfb-toolbar" role="group" aria-label={`${SIDE_LABEL[side]} pane actions`}>
          {download ? (
            <span className="dfb-tool">
              <Spinner size={16} label={`Preparing… ${elapsed}s`} />
              <Button size="sm" variant="ghost" onClick={() => download.ctrl.abort()}>
                Cancel
              </Button>
            </span>
          ) : (
            button("download", toolbar.download, "Download", <Icon name="download" size={16} />, onDownload)
          )}
          {button(
            "copy",
            toolbar.copyOther,
            toolbar.copyOtherLabel,
            // Points at the destination pane, whichever way the panes are laid out.
            <Icon name="chevron-right" size={16} className={`dfb-copy-icon dfb-copy-icon--${side}`} />,
            () => onAction("copyOther"),
          )}
          {button("same", toolbar.copySame, "Copy to same location", <Icon name="share" size={16} />, () =>
            onAction("copySame"),
          )}
          {button("upload", toolbar.upload, "Upload", <Icon name="upload" size={16} />, () =>
            // Inside the click handler: a phone opens its picker only from a
            // user gesture.
            fileRef.current?.click(),
          )}
          {button("mkdir", toolbar.newFolder, "New folder", <Icon name="add" size={16} />, () =>
            onAction("mkdir"),
          )}
          {button("rename", toolbar.rename, "Rename", <Icon name="rename" size={16} />, () =>
            onAction("rename"),
          )}
          {button("delete", toolbar.delete, "Delete", <Icon name="trash" size={16} />, () =>
            onAction("delete"),
          )}
          {/* One file per pick. Hidden, so it is neither in the tab order nor in
              the accessibility tree beside the Upload button. */}
          <input
            ref={fileRef}
            type="file"
            hidden
            tabIndex={-1}
            onChange={(e) => {
              const file = e.target.files?.[0];
              // Reset, so picking the same file again still fires a change.
              e.target.value = "";
              if (file) onUpload(file);
            }}
          />
          {/* Always mounted, so a change is announced. A refused rename is
              said with the selection; a watched folder's "can't be renamed"
              once, on entering it, until the first selection replaces it. */}
          <span className="text-sm text-muted mono" aria-live="polite">
            {selectedEntries.length > 0 ? selectionText(selectedEntries) : folderNote}
            {toolbar.renameRefusal && (
              <span className="sr-only">{`. ${toolbar.renameRefusal}`}</span>
            )}
          </span>
        </div>
      )}
      {toolbar.paneReasons.map((r) => (
        <p key={r} className="text-sm text-muted dfb-reason">
          {r}
        </p>
      ))}
      {error && (
        <p className="notice tone-problem" role="alert">
          <Icon name="problem" size={18} />
          <span>{error}</span>
        </p>
      )}

      <div
        ref={listRef}
        tabIndex={-1}
        className="dfb-listing"
        role="region"
        aria-label={
          unknownRig
            ? listingRegionLabel(side, "", null, "")
            : listingRegionLabel(side, rigName, place.share, place.dir)
        }
      >
        {listing}
      </div>
    </section>
  );
}

function Crumb({
  label,
  current,
  onClick,
}: {
  label: string;
  current: boolean;
  onClick: () => void;
}) {
  return (
    <li className="dfb-crumbs__item">
      <button
        type="button"
        className="dfb-link break-anywhere"
        aria-current={current ? "location" : undefined}
        onClick={onClick}
      >
        {label}
      </button>
    </li>
  );
}

/* ---- new folder ------------------------------------------------------------ */

function NewFolderDialog({
  rig,
  rigName,
  share,
  dir,
  onClose,
  onCreated,
}: {
  rig: string;
  rigName: string;
  share: string;
  dir: string;
  onClose: () => void;
  onCreated: (name: string) => void;
}) {
  const [name, setName] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const formId = useId();

  const submit = async () => {
    const invalid = validateName(name, "folder");
    if (invalid) {
      setError(invalid);
      return;
    }
    setSaving(true);
    setError(null);
    try {
      await makeDeviceFolder(rig, share, dir, name);
      onCreated(name);
    } catch (err) {
      setError(
        err instanceof ApiError && err.code === "exists"
          ? `A file or folder named ${name} already exists here.`
          : errText(err),
      );
      setSaving(false);
    }
  };

  return (
    <Dialog
      open
      onClose={() => !saving && onClose()}
      title={`New folder in ${rigName} › ${dir ? `${share}/${dir}` : share}`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={saving}>
            Cancel
          </Button>
          <Button variant="primary" type="submit" form={formId} loading={saving}>
            Create folder
          </Button>
        </>
      }
    >
      <form
        id={formId}
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        <Field label="Folder name" error={error}>
          {(control) => (
            <TextInput
              {...control}
              value={name}
              invalid={!!error}
              autoComplete="off"
              onChange={(e) => {
                setName(e.target.value);
                setError(null);
              }}
            />
          )}
        </Field>
      </form>
    </Dialog>
  );
}

/* ---- rename ---------------------------------------------------------------- */

export function RenameDialog({
  rig,
  rigName,
  share,
  dir,
  name,
  isDir,
  onClose,
  onRelist,
  onRenamed,
}: {
  rig: string;
  rigName: string;
  share: string;
  dir: string;
  name: string;
  isDir: boolean;
  onClose: () => void;
  onRelist: () => void;
  onRenamed: (newName: string) => void;
}) {
  const [value, setValue] = useState(name);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  /** The source is gone (or already renamed): only Close is left. */
  const [gone, setGone] = useState(false);
  const formId = useId();
  const inputRef = useRef<HTMLInputElement>(null);
  const waiting = useWaiting(saving, 0);

  // Typing keeps the extension: the stem is selected, and the Dialog focuses
  // the field (initialFocus) without moving the selection.
  useEffect(() => {
    const [start, end] = renameSelection(name, isDir);
    inputRef.current?.setSelectionRange(start, end);
  }, [name, isDir]);

  const submit = async () => {
    // Validated on submit only, so a name passing through an invalid state as
    // it is typed ("Dark " on the way to "Dark Library") is never announced.
    const invalid = value === name ? RENAME_UNCHANGED : validateName(value, "any");
    if (invalid) {
      setError(invalid);
      return;
    }
    setSaving(true);
    setError(null);
    try {
      await renameDeviceEntry(rig, { share, dir, name, new_name: value });
      onRenamed(value);
    } catch (err) {
      setSaving(false);
      if (err instanceof ApiError) {
        setError(renameErrorText(err.code, err.message, { rigName, name, newName: value }));
        if (err.code === "not_found") {
          // A second submit could only answer not_found again; the panes show
          // the folder as it now is.
          setGone(true);
          onRelist();
        }
      } else {
        setError(errText(err));
      }
    }
  };

  return (
    <Dialog
      open
      onClose={() => !saving && onClose()}
      dismissOnScrim={!saving}
      title={`Rename ${name}`}
      initialFocus={inputRef}
      footer={
        gone ? (
          <Button variant="secondary" onClick={onClose}>
            Close
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose} disabled={saving}>
              Cancel
            </Button>
            <Button variant="primary" type="submit" form={formId} loading={saving}>
              Rename
            </Button>
          </>
        )
      }
    >
      <form
        id={formId}
        className="stack"
        onSubmit={(e) => {
          e.preventDefault();
          if (!gone && !saving) void submit();
        }}
      >
        <p className="text-sm text-muted break-anywhere">in {placeText(rigName, share, dir)}</p>
        <Field label="New name" error={error}>
          {(control) => (
            <TextInput
              {...control}
              ref={inputRef}
              value={value}
              invalid={!!error}
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => {
                setValue(e.target.value);
                if (!gone) setError(null);
              }}
            />
          )}
        </Field>
        <LiveText className="text-sm text-muted" text={waiting ? waitingText([rigName]) : ""} />
      </form>
    </Dialog>
  );
}

/* ---- run plumbing shared by copy, upload and delete ----------------------- */

type Phase = "checking" | "planFailed" | "plan" | "progress" | "report";

interface RunResult {
  rows: ReportRow[];
  end: RunEnd;
  summary: FilesSummaryLine | null;
}

/**
 * Drives one streamed run and builds its report rows. A refusal before the first
 * line (busy, unarchived, a bad request) rethrows as the ApiError it is; anything
 * that cuts the stream after that is a stopped or interrupted run. `onSettled`,
 * when given, receives the rows that already have their result after each line.
 */
async function executeRun(
  start: (onLine: (line: FilesRunLine) => void, signal: AbortSignal) => Promise<void>,
  order: string[],
  ctrl: AbortController,
  stopped: () => boolean,
  onProgress: (p: FilesProgressLine | null) => void,
  onSettled?: (rows: ReportRow[]) => void,
): Promise<RunResult> {
  const lines: FilesItemLine[] = [];
  let lastCurrent: string | null = null;
  let summary: FilesSummaryLine | null = null;
  try {
    await start((line) => {
      if (line.type === "progress") {
        lastCurrent = line.current;
        onProgress(line);
      } else if (line.type === "item") {
        lines.push(line);
        onProgress(null);
      } else {
        summary = line;
        onProgress(null);
      }
      if (onSettled && line.type !== "summary") onSettled(settledRows(order, lines, lastCurrent));
    }, ctrl.signal);
  } catch (err) {
    if (err instanceof ApiError) throw err;
    // Aborted by Stop, or the connection dropped: the report is built below.
  }
  const merged = mergeRunReport({
    order,
    lines,
    lastCurrent,
    summary,
    stoppedByUser: stopped(),
  });
  return { ...merged, summary };
}

/** Moves focus as the dialog's content changes: Stop while sending or during
 * progress, else the title, so focus never falls out of the dialog when a
 * button unmounts. */
function usePhaseFocus(phase: Phase | "sending" | "sendFailed", stopId: string) {
  const titleRef = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    if (phase === "progress" || phase === "sending") document.getElementById(stopId)?.focus();
    else titleRef.current?.focus();
  }, [phase, stopId]);
  return titleRef;
}

/**
 * Text in a polite live region that is mounted empty and filled afterwards: a
 * region inserted with its text already in it is not announced by every screen
 * reader.
 */
function LiveText({ text, className }: { text: string; className?: string }) {
  const [shown, setShown] = useState("");
  useEffect(() => {
    setShown(text);
  }, [text]);
  return (
    <span className={className} aria-live="polite">
      {shown}
    </span>
  );
}

/** "Checking 2 selected items on Alpha… 3s": the seconds stay out of the live
 * text, which would otherwise be re-read every second. */
function CheckingLine({ text, elapsed }: { text: string; elapsed: number }) {
  return (
    <span className="cluster" style={{ ["--cluster-gap" as string]: "var(--space-2)" }}>
      <span aria-hidden="true">
        <Spinner />
      </span>
      <LiveText className="text-muted text-sm" text={text} />
      <span className="text-muted text-sm mono">{elapsed}s</span>
    </span>
  );
}

function ProgressView({
  counter,
  bytes,
  pct,
  current,
  waiting,
  label,
  note,
}: {
  counter: string;
  bytes: string | null;
  pct: number;
  current: string;
  waiting: string | null;
  /** The bar's accessible name. */
  label: string;
  note: string;
}) {
  return (
    <div className="stack dfb-progress">
      <p>
        <LiveText text={counter} />
        {bytes && <span className="mono text-sm"> · {bytes}</span>}
        {waiting && <span className="text-sm text-muted"> {waiting}</span>}
      </p>
      <div
        className="dfb-bar"
        role="progressbar"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={pct}
      >
        <span style={{ width: `${pct}%` }} />
      </div>
      {current && <p className="mono text-sm break-anywhere">{current}</p>}
      <p className="text-sm text-muted">{note}</p>
    </div>
  );
}

const keepOpenNote = (op: RunOp) =>
  `Keep this page open; leaving it or locking the phone stops the ${op}.`;

const pct = (part: number, whole: number) =>
  whole > 0 ? Math.min(100, Math.round((part / whole) * 100)) : 0;

/** The progress of a copy or an upload (files and bytes) or a delete (items),
 * for the Progress phase and a report's row action alike. An upload's one file
 * is named in its counter, given as `upload`. */
function runProgressView(
  op: RunOp,
  progress: FilesProgressLine | null,
  fallback: { total: number; bytes: number },
  waiting: string | null,
  upload?: { name: string; rigName: string },
): ReactNode {
  const done = progress?.done ?? 0;
  const total = progress?.total ?? fallback.total;
  if (op === "upload" && upload) {
    const bytesTotal = progress?.bytes_total ?? fallback.bytes;
    return (
      <ProgressView
        label={PROGRESS_LABEL.upload}
        note={keepOpenNote("upload")}
        counter={uploadWritingText(upload.name, upload.rigName)}
        bytes={`${formatBytes(progress?.bytes_done ?? 0)} of ${formatBytes(bytesTotal)}`}
        pct={pct(progress?.bytes_done ?? 0, bytesTotal)}
        current=""
        waiting={waiting}
      />
    );
  }
  if (op === "copy") {
    const bytesTotal = progress?.bytes_total ?? fallback.bytes;
    return (
      <ProgressView
        label={PROGRESS_LABEL.copy}
        note={keepOpenNote("copy")}
        counter={`File ${Math.min(done + 1, Math.max(total, 1))} of ${total}`}
        bytes={`${formatBytes(progress?.bytes_done ?? 0)} of ${formatBytes(bytesTotal)}`}
        pct={pct(progress?.bytes_done ?? 0, bytesTotal)}
        current={progress?.current ?? ""}
        waiting={waiting}
      />
    );
  }
  return (
    <ProgressView
      label={PROGRESS_LABEL.delete}
      note={keepOpenNote("delete")}
      counter={`Deleted ${done} of ${total} items`}
      bytes={null}
      pct={pct(done, total)}
      current={progress?.current ?? ""}
      waiting={waiting}
    />
  );
}

/**
 * A report's row actions (Retry, Overwrite, Retry N): one run at a time, behaving
 * like Progress while it lasts (the dialog ignores Esc, the scrim and its X; Stop
 * is the way out). Rows update as their lines arrive; when the run ends, its rows'
 * outcome and the report's heading are announced and focus moves to Close.
 */
function useRowRun({
  op,
  rows,
  heading,
  setRows,
  setEnd,
  setAborted,
  onRunning,
  onRelist,
  stopId,
  closeId,
}: {
  op: RunOp;
  rows: ReportRow[];
  heading: string;
  setRows: (f: (prev: ReportRow[]) => ReportRow[]) => void;
  setEnd: (end: RunEnd) => void;
  setAborted: (aborted: FilesSummaryLine["aborted"]) => void;
  onRunning: (running: boolean) => void;
  onRelist: () => void;
  stopId: string;
  closeId: string;
}) {
  /** Rels still waiting for their result, or null when no row action runs. */
  const [running, setRunning] = useState<Set<string> | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [progress, setProgress] = useState<FilesProgressLine | null>(null);
  const [seq, setSeq] = useState(0);
  const [total, setTotal] = useState(0);
  const [finished, setFinished] = useState<{ rels: string[]; ok: boolean } | null>(null);
  const [live, setLive] = useState("");

  const run = async (
    rels: string[],
    execute: (
      onProgress: (p: FilesProgressLine | null) => void,
      onSettled: (rows: ReportRow[]) => void,
    ) => Promise<RunResult>,
  ) => {
    setError(null);
    setProgress(null);
    setTotal(rels.length);
    setRunning(new Set(rels));
    onRunning(true);
    let ok = false;
    try {
      const result = await execute(
        (p) => {
          setSeq((n) => n + 1);
          if (p) setProgress(p);
        },
        (settled) => {
          setRows((prev) => applyRowUpdates(prev, settled));
          const done = new Set(settled.map((r) => r.rel));
          setRunning(new Set(rels.filter((r) => !done.has(r))));
        },
      );
      setRows((prev) => applyRowUpdates(prev, result.rows));
      // The heading follows the latest run: a retry that finishes cleanly turns
      // "Copy stopped" into "Copied N of M".
      setEnd(result.end);
      setAborted(result.summary?.aborted ?? "");
      ok = true;
    } catch (err) {
      setError(errText(err));
    } finally {
      setRunning(null);
      setFinished({ rels, ok });
      onRunning(false);
      onRelist();
    }
  };

  const active = running !== null;
  useEffect(() => {
    if (active) document.getElementById(stopId)?.focus();
  }, [active, stopId]);

  // Runs once the run's rows and the heading they produce are rendered, so the
  // announcement says what the dialog now shows.
  useEffect(() => {
    if (!finished) return;
    if (finished.ok) {
      const done = rows.filter((r) => finished.rels.includes(r.rel));
      setLive(`${rowRunAnnouncement(done, op)} ${heading}`);
    }
    document.getElementById(closeId)?.focus();
    setFinished(null);
  }, [finished]); // eslint-disable-line react-hooks/exhaustive-deps

  return { running, error, progress, seq, total, live, run };
}

interface ReportViewProps {
  op: RunOp;
  rows: ReportRow[];
  listed: string[];
  /** The report row's wording context. */
  rigFor: (row: ReportRow) => string;
  sourceRig?: string;
  address?: string;
  /** An upload's last run of this row was bound to overwrite. */
  overwriting?: (row: ReportRow) => boolean;
  /** Rels with an action running, or null. */
  runningRels: Set<string> | null;
  /** The running row action's progress, shown above the rows. */
  progress: ReactNode;
  waiting: string | null;
  error: string | null;
  confirmRel: string | null;
  confirmText: (row: ReportRow) => string;
  onOverwrite: (row: ReportRow) => void;
  onConfirm: (row: ReportRow) => void;
  onCancelConfirm: () => void;
  onRetry: (rows: ReportRow[]) => void;
  counts: ReportCounts;
  doneLabel: string;
}

function rowLabel(row: ReportRow, op: RunOp): { tone: "safe" | "neutral" | "problem"; icon: "safe" | "pause" | "problem"; label: string } {
  const label = rowOutcomeLabel(row, op);
  if (row.outcome === "done") return { tone: "safe", icon: "safe", label };
  if (row.outcome === "skipped") return { tone: "neutral", icon: "pause", label };
  return { tone: "problem", icon: "problem", label };
}

function ReportView(props: ReportViewProps) {
  const { op, rows, listed, runningRels, counts } = props;
  const uid = useId();
  const byRel = new Map(rows.map((r) => [r.rel, r]));
  const shown = listed.map((rel) => byRel.get(rel)!).filter(Boolean);
  const actionId = (rel: string) => `${uid}-act-${listed.indexOf(rel)}`;
  const reason = (row: ReportRow): { text: string; action: RowAction } => {
    if (!row.code) return { text: "", action: null };
    const r = reasonText(row.code, {
      op,
      rig: props.rigFor(row),
      sourceRig: props.sourceRig,
      name: baseName(row.rel),
      address: props.address,
      message: row.message,
      cause: row.cause,
      overwriting: props.overwriting?.(row),
    });
    // A folder the run could not create is no item a row action can send.
    return row.dir ? { ...r, action: null } : r;
  };
  const retryable = shown.filter((r) => r.outcome !== "done" && reason(r).action === "retry");

  // The inline confirmation replaces the row's Overwrite button: focus moves to
  // its primary button, and back to Overwrite when it is cancelled.
  const confirmId = `${uid}-confirm`;
  const lastConfirm = useRef<string | null>(null);
  useEffect(() => {
    if (props.confirmRel) {
      lastConfirm.current = props.confirmRel;
      document.getElementById(confirmId)?.focus();
    } else if (lastConfirm.current && runningRels === null) {
      document.getElementById(actionId(lastConfirm.current))?.focus();
      lastConfirm.current = null;
    } else {
      lastConfirm.current = null;
    }
  }, [props.confirmRel]); // eslint-disable-line react-hooks/exhaustive-deps

  const columns: Column<ReportRow>[] = [
    {
      key: "file",
      header: "File",
      cell: (r) => (
        <span className="mono text-sm break-anywhere">
          {r.files !== undefined && r.files > 0 ? `${r.rel} (${r.files} files)` : r.rel}
        </span>
      ),
    },
    {
      key: "outcome",
      header: "Outcome",
      cell: (r) =>
        runningRels?.has(r.rel) ? (
          <span className="stack">
            <Spinner size={16} label={RUNNING_ROW_LABEL[op]} />
            {props.waiting && <span className="text-xs text-muted">{props.waiting}</span>}
          </span>
        ) : (
          <StatusPill {...rowLabel(r, op)} />
        ),
    },
    {
      key: "reason",
      header: "Reason",
      cell: (r) => <span className="text-sm">{reason(r).text}</span>,
    },
    {
      key: "action",
      header: "Action",
      cell: (r) => {
        if (r.outcome === "done") return null;
        const { action } = reason(r);
        if (!action) return null;
        const disabled = runningRels !== null;
        if (action === "overwrite" && props.confirmRel === r.rel) {
          const questionId = `${uid}-q`;
          return (
            <div className="stack dfb-confirm">
              <p className="text-sm" id={questionId}>
                {props.confirmText(r)}
              </p>
              <span className="cluster">
                <Button
                  id={confirmId}
                  size="sm"
                  variant="danger"
                  disabled={disabled}
                  aria-describedby={questionId}
                  onClick={() => props.onConfirm(r)}
                >
                  Overwrite
                </Button>
                <Button size="sm" variant="ghost" onClick={props.onCancelConfirm}>
                  Cancel
                </Button>
              </span>
            </div>
          );
        }
        return action === "overwrite" ? (
          <Button
            id={actionId(r.rel)}
            size="sm"
            variant="secondary"
            disabled={disabled}
            aria-label={`Overwrite ${r.rel}`}
            onClick={() => props.onOverwrite(r)}
          >
            Overwrite
          </Button>
        ) : (
          <Button
            id={actionId(r.rel)}
            size="sm"
            variant="secondary"
            disabled={disabled}
            aria-label={`Retry ${r.rel}`}
            onClick={() => props.onRetry([r])}
          >
            Retry
          </Button>
        );
      },
    },
  ];

  return (
    <div className="stack dfb-report">
      <div className="cluster">
        <StatusPill tone="safe" icon="safe" label={`${counts.done} ${props.doneLabel}`} />
        <StatusPill tone="neutral" icon="pause" label={`${counts.skipped} skipped`} />
        <StatusPill tone="problem" icon="problem" label={`${counts.failed} failed`} />
        {counts.notAttempted > 0 && (
          <StatusPill tone="problem" icon="problem" label={`${counts.notAttempted} not attempted`} />
        )}
      </div>
      {props.error && (
        <p className="notice tone-problem" role="alert">
          <Icon name="problem" size={18} />
          <span>{props.error}</span>
        </p>
      )}
      {props.progress}
      {retryable.length >= 2 && (
        <div>
          <Button
            variant="secondary"
            size="sm"
            disabled={runningRels !== null}
            onClick={() => props.onRetry(retryable)}
          >
            Retry {retryable.length} failed and not attempted
          </Button>
        </div>
      )}
      {shown.length > 0 && (
        <Table
          caption="Items not completed, and notes"
          columns={columns}
          rows={shown}
          getKey={(r) => r.rel}
          className="dfb-stack-table"
        />
      )}
    </div>
  );
}

/* ---- copy ----------------------------------------------------------------- */

interface CopyFrom {
  rig: string;
  rigName: string;
  share: string;
  dir: string;
  names: string[];
}
interface CopyTo {
  rig: string;
  rigName: string;
  share: string;
  dir: string;
}

/** Where the copy dialog's bytes come from: a selection on a rig, or one file
 * picked on this computer (an upload, sent to Commander first). */
export type CopySource =
  | { kind: "copy"; from: CopyFrom; mirror: boolean }
  | { kind: "upload"; file: File };

/** The Sending state's bar moves at most once a second. */
const SENDING_UPDATE_MS = 1000;

/** A staging refusal (Commander's, or a proxy's in front of it), worded for the
 * dialog. An ApiError, so a row action shows it like any refusal. */
class StageRefusal extends ApiError {
  constructor(err: ApiError) {
    super(
      err.status,
      // A proxy answers without Commander's JSON body; a 401 has its own line.
      err.body === undefined && err.status !== 401 ? nonJsonUploadRefusal(err.status) : err.message,
      { code: err.code, body: err.body },
    );
  }
}

/** An upload's run has one file: a run cut off before its summary reports it
 * stopped, whose wording covers either outcome, not "not attempted". */
function uploadRunResult(result: RunResult): RunResult {
  if (result.end === "summary") return result;
  return {
    ...result,
    rows: result.rows.map((r) =>
      r.code === "not_attempted" ? { ...r, code: "stopped", cause: undefined } : r,
    ),
  };
}

export function CopyDialog({
  source,
  to,
  onClose,
  onRunning,
  onRelist,
}: {
  source: CopySource;
  to: CopyTo;
  onClose: () => void;
  /** True while a run streams (or an upload is sent), for the page's leave guards. */
  onRunning: (running: boolean) => void;
  onRelist: () => void;
}) {
  const toast = useToast();
  const stopId = useId();
  const closeId = useId();
  const skipReasonId = useId();
  const from = source.kind === "copy" ? source.from : null;
  const mirror = source.kind === "copy" && source.mirror;
  const file = source.kind === "upload" ? source.file : null;
  const op: RunOp = file ? "upload" : "copy";
  const [phase, setPhase] = useState<Phase>("checking");
  const [plan, setPlan] = useState<UploadPlan | null>(null);
  const [planError, setPlanError] = useState<string | null>(null);
  /** Whether Plan failed offers Try again: a PC-side name can't be fixed here. */
  const [planRetry, setPlanRetry] = useState(true);
  const [runError, setRunError] = useState<string | null>(null);
  const [decisions, setDecisions] = useState<Decisions>({});
  /** Set once the user changes a decision, so the plan's new totals are read out. */
  const decided = useRef(false);
  const dispatch = useCallback((action: DecisionAction) => {
    decided.current = true;
    setConfirming(false);
    setDecisions((d) => conflictDecisions(d, action));
  }, []);
  const [checkStart, setCheckStart] = useState<number | null>(null);
  const elapsed = useElapsed(checkStart);
  const [progress, setProgress] = useState<FilesProgressLine | null>(null);
  const [seq, setSeq] = useState(0);
  const [rows, setRows] = useState<ReportRow[]>([]);
  const [listed, setListed] = useState<string[]>([]);
  const [end, setEnd] = useState<RunEnd>("summary");
  const [aborted, setAborted] = useState<FilesSummaryLine["aborted"]>("");
  const [confirmRel, setConfirmRel] = useState<string | null>(null);
  const [planLive, setPlanLive] = useState("");
  const ctrlRef = useRef<AbortController | null>(null);
  const stoppedRef = useRef(false);
  const crossRig = !!from && from.rig !== to.rig;

  // ---- an upload's staging (phase 1) ----
  /** The staged file's id, once Commander holds the whole file. */
  const idRef = useRef<string | null>(null);
  /** Bytes sent while the Sending state shows over the dialog's phase. */
  const [sending, setSending] = useState<{ loaded: number; total: number } | null>(null);
  const [sendFailed, setSendFailed] = useState(false);
  const stageWaiter = useRef<{ resolve: (id: string) => void; reject: (e: unknown) => void } | null>(
    null,
  );
  /** Rels whose latest run was bound to overwrite, for the stopped wording. */
  const overwritten = useRef<Set<string>>(new Set());
  /** The inline confirmation before an upload overwrites an unarchived file. */
  const [confirming, setConfirming] = useState(false);

  // Sending and running both hold the page's leave guards; a row action's run
  // may re-send the file inside it, so the guard counts rather than toggles.
  const busyCount = useRef(0);
  const setBusy = useCallback(
    (on: boolean) => {
      busyCount.current = Math.max(0, busyCount.current + (on ? 1 : -1));
      onRunning(busyCount.current > 0);
    },
    [onRunning],
  );

  const view = sending ? "sending" : sendFailed ? "sendFailed" : phase;
  const titleRef = usePhaseFocus(view, stopId);

  useEffect(
    () => () => {
      ctrlRef.current?.abort();
      // Closing the dialog drops the staged file, and with it the Retry; the
      // server's expiry covers a request that never arrives.
      if (idRef.current) void discardUpload(idRef.current).catch(() => {});
    },
    [],
  );

  const fresh = () => {
    const ctrl = new AbortController();
    ctrlRef.current = ctrl;
    return ctrl;
  };

  /** One send of the file to Commander. A lost connection waits on Send again;
   * Stop closes the dialog; a refusal rejects the pending stage(). */
  const send = () => {
    if (!file) return;
    const ctrl = fresh();
    setSendFailed(false);
    setSending({ loaded: 0, total: file.size });
    setBusy(true);
    let last = 0;
    stageUpload(
      file,
      (loaded, total) => {
        const now = Date.now();
        if (now - last < SENDING_UPDATE_MS && loaded < total) return;
        last = now;
        setSending({ loaded, total });
      },
      ctrl.signal,
    ).then(
      (staged) => {
        setBusy(false);
        idRef.current = staged.id;
        setSending(null);
        stageWaiter.current?.resolve(staged.id);
      },
      (err) => {
        setBusy(false);
        if (isAbort(err)) return;
        setSending(null);
        if (err instanceof ApiError) stageWaiter.current?.reject(new StageRefusal(err));
        else setSendFailed(true);
      },
    );
  };

  const stage = () =>
    new Promise<string>((resolve, reject) => {
      stageWaiter.current = { resolve, reject };
      send();
    });

  /** Runs `request` with the staged id, sending the file first when Commander
   * holds none. A staged file that is gone (expired, evicted, lost to a
   * restart) is sent again from the File the dialog still holds, and the
   * request repeated. */
  const withStaged = async <T,>(request: (id: string) => Promise<T>): Promise<T> => {
    const id = idRef.current ?? (await stage());
    try {
      return await request(id);
    } catch (err) {
      if (!(err instanceof ApiError && err.code === "staging_gone")) throw err;
      idRef.current = null;
      return request(await stage());
    }
  };

  const stopSending = () => {
    // Nothing was staged and nothing reached the device. The dialog closes, so
    // the outcome goes to the polite toast region.
    ctrlRef.current?.abort();
    toast.info(sendStoppedText(to.rigName));
    onClose();
  };

  const place = { rig: to.rig, share: to.share, dir: to.dir };

  const check = useCallback(async () => {
    setPhase("checking");
    setPlanError(null);
    setPlanRetry(true);
    try {
      const p = file
        ? await withStaged((id) => {
            setCheckStart(Date.now());
            return planDeviceUpload({ id, to: place }, fresh().signal);
          })
        : await (() => {
            setCheckStart(Date.now());
            return planDeviceCopy(
              {
                from: { rig: from!.rig, share: from!.share, dir: from!.dir, names: from!.names },
                to: place,
                mirror,
              },
              fresh().signal,
            );
          })();
      setPlan(p);
      setDecisions(initialDecisions(p.conflicts));
      setConfirming(false);
      decided.current = false;
      setPhase("plan");
    } catch (err) {
      if (isAbort(err)) return;
      // Sending the same name again meets the same refusal.
      if (err instanceof StageRefusal && err.code === "bad_path") setPlanRetry(false);
      setPlanError(
        err instanceof StageRefusal
          ? err.message
          : err instanceof ApiError
            ? planFailedText(op, err.code, err.message, from?.rigName ?? "", to.rigName)
            : planFailedText(op, undefined, errText(err), from?.rigName ?? "", to.rigName),
      );
      setPhase("planFailed");
    } finally {
      setCheckStart(null);
    }
  }, [from, to, mirror]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    // A name the device would refuse fails before anything is sent.
    const problem = file ? uploadPickProblem(file.name, to.rigName) : null;
    if (problem) {
      setPlanError(problem);
      setPlanRetry(false);
      setPhase("planFailed");
      return;
    }
    void check();
    // Checked once on open; Try again re-runs it.
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const sel = plan ? copySelection(plan, decisions) : null;
  const itemByRel = useMemo(
    () => new Map((plan?.items ?? []).map((it) => [it.rel, it])),
    [plan],
  );
  const chosenOverwrite = useMemo(
    () => new Map((sel?.overwrite ?? []).map((o) => [o.rel, o])),
    [sel],
  );
  /** The upload's one file as the server names it. */
  const uploadRel = plan?.items[0]?.rel ?? file?.name ?? "";
  const uploadSize = plan?.items[0]?.size ?? file?.size ?? 0;

  const runBody = (items: string[], overwrite: CopyOverwrite[], dirs: string[]) => ({
    from: { rig: from!.rig, share: from!.share, dir: from!.dir },
    to: place,
    dirs,
    items: items.map((rel) => ({ rel, size: itemByRel.get(rel)?.size ?? 0 })),
    overwrite,
  });

  /** One upload run of the staged file, bound to overwrite when `ow` is set. */
  const executeUpload = (
    ow: CopyOverwrite | undefined,
    onProgress: (p: FilesProgressLine | null) => void,
    onSettled?: (rows: ReportRow[]) => void,
  ) => {
    if (ow) overwritten.current.add(uploadRel);
    else overwritten.current.delete(uploadRel);
    return withStaged((id) => {
      const ctrl = fresh();
      stoppedRef.current = false;
      return executeRun(
        (onLine, signal) =>
          runDeviceUpload(
            { id, to: place, ...(ow ? { dst_size: ow.dst_size, dst_mtime: ow.dst_mtime } : {}) },
            onLine,
            signal,
          ),
        [uploadRel],
        ctrl,
        () => stoppedRef.current,
        onProgress,
        onSettled,
      );
    }).then(uploadRunResult);
  };

  const onLineProgress = (p: FilesProgressLine | null) => {
    setSeq((n) => n + 1);
    if (p) setProgress(p);
  };

  const start = async () => {
    if (!plan || !sel) return;
    stoppedRef.current = false;
    setConfirming(false);
    setRunError(null);
    setProgress(null);
    setPhase("progress");
    setBusy(true);
    const order = sel.items.map((i) => i.rel);
    let result: RunResult;
    try {
      result = file
        ? await executeUpload(sel.overwrite[0], onLineProgress)
        : await executeRun(
            (onLine, signal) => runDeviceCopy(runBody(order, sel.overwrite, sel.dirs), onLine, signal),
            order,
            fresh(),
            () => stoppedRef.current,
            onLineProgress,
          );
    } catch (err) {
      // A refusal before any line: nothing was written, and a staged file is kept.
      setBusy(false);
      setRunError(errText(err));
      setPhase("plan");
      return;
    }
    setBusy(false);
    onRelist();

    // Skips the user chose, and type conflicts, come from the plan itself.
    const clientRows: ReportRow[] = [
      ...plan.conflicts
        .filter((c) => c.kind === "file" && !chosenOverwrite.has(c.rel))
        .map(
          (c): ReportRow => ({
            rel: c.rel,
            outcome: "skipped",
            code: "chose_skip",
            dst_size: c.dst_size,
            dst_mtime: c.dst_mtime,
            dst_ledger: c.dst_ledger,
          }),
        ),
      ...sel.typeConflicts.map(
        (t): ReportRow => ({
          rel: t.rel,
          outcome: "skipped",
          code: "type_conflict",
          files: t.isDir ? t.files : undefined,
        }),
      ),
    ];
    const all = [...clientRows, ...result.rows];
    const listedRels = all.filter(isListed).map((r) => r.rel);
    if (result.end === "summary" && listedRels.length === 0) {
      toast.success(
        file
          ? uploadDoneText(uploadRel, uploadSize, to.rigName)
          : copyDoneText(sel, to.rigName),
      );
      onClose();
      return;
    }
    setRows(all);
    setListed(listedRels);
    setEnd(result.end);
    setAborted(result.summary?.aborted ?? "");
    setPhase("report");
  };

  const stop = () => {
    stoppedRef.current = true;
    ctrlRef.current?.abort();
  };

  const counts = reportCounts(rows);
  const total = plan?.items.length ?? 0;
  // The rig whose failure ended the run, which its not-attempted rows name.
  const abortRig = rigNamedIn(
    rows.find((r) => r.code === "unreachable" || r.code === "identity")?.message,
    from ? [from.rigName, to.rigName] : [to.rigName],
    to.rigName,
  );
  const reportTitle = file
    ? uploadReportTitle(end, rows.find((r) => r.rel === uploadRel)?.outcome, uploadRel, to.rigName)
    : end === "stopped"
      ? "Copy stopped"
      : end === "interrupted"
        ? "Copy interrupted — lost connection to Commander"
        : total === 0 && sel
          ? // A folders-only copy: its rows are the folders it could not create.
            `Created ${sel.createFolders - rows.filter((r) => r.dir).length} of ${sel.createFolders} folder${sel.createFolders === 1 ? "" : "s"} on ${to.rigName}`
          : `Copied ${counts.done} of ${total} file${total === 1 ? "" : "s"} to ${to.rigName}` +
            abortedSuffix(aborted, abortRig);

  const rowRun = useRowRun({
    op,
    rows,
    heading: reportTitle,
    setRows,
    setEnd,
    setAborted,
    onRunning: setBusy,
    onRelist,
    stopId,
    closeId,
  });

  const waiting = useWaiting(phase === "progress" || rowRun.running !== null, seq + rowRun.seq);
  const waitText = waiting
    ? waitingText(crossRig ? [from!.rigName, to.rigName] : [to.rigName])
    : null;

  const runRows = (target: ReportRow[], overwriteRow?: ReportRow) => {
    if (!plan || !sel) return;
    const rels = target.map((r) => r.rel);
    const overwrite: CopyOverwrite[] = overwriteRow
      ? [
          {
            rel: overwriteRow.rel,
            dst_size: overwriteRow.dst_size ?? 0,
            dst_mtime: overwriteRow.dst_mtime ?? 0,
          },
        ]
      : // A retried item the user chose to overwrite keeps that choice, bound to
        // the state the user saw.
        rels.flatMap((rel) => (chosenOverwrite.has(rel) ? [chosenOverwrite.get(rel)!] : []));
    setConfirmRel(null);
    if (file) {
      // Retry and Overwrite write the staged file again; nothing is re-sent
      // from this computer unless Commander no longer holds it.
      void rowRun.run(rels, (onProgress, onSettled) =>
        executeUpload(overwrite[0], onProgress, onSettled),
      );
      return;
    }
    const needed = new Set(rels.flatMap(ancestorsOf).map((d) => d.toLowerCase()));
    const dirs = sel.dirs.filter((d) => needed.has(d.toLowerCase()));
    const ctrl = fresh();
    stoppedRef.current = false;
    void rowRun.run(rels, (onProgress, onSettled) =>
      executeRun(
        (onLine, signal) => runDeviceCopy(runBody(rels, overwrite, dirs), onLine, signal),
        rels,
        ctrl,
        () => stoppedRef.current,
        onProgress,
        onSettled,
      ),
    );
  };

  const planTitle = sel ? copyPlanTitle(sel, to.rigName, mirror) : "";
  const overwriting = !!file && !!sel && sel.overwrite.length > 0;
  const buttonLabel = file
    ? sel && sel.files > 0
      ? overwriting
        ? "Upload and overwrite"
        : "Upload"
      : ""
    : sel
      ? copyButtonLabel(sel)
      : "";
  const title = file
    ? (phase === "plan" || phase === "progress") && sel && !sending && !sendFailed
      ? uploadPlanTitle(uploadRel, uploadSize, to.rigName)
      : phase === "report"
        ? reportTitle
        : `Upload ${file.name} to ${to.rigName}`
    : (phase === "plan" || phase === "progress") && sel
      ? planTitle
      : phase === "report"
        ? reportTitle
        : mirror
          ? `Copy to the same location on ${to.rigName}`
          : `Copy to ${to.rigName}`;

  // A decision changes the totals in the title and the button: read them out
  // once the user pauses, rather than on every click of Overwrite all.
  useEffect(() => {
    if (phase !== "plan" || !decided.current) return;
    const t = setTimeout(
      () =>
        setPlanLive(
          file ? `${buttonLabel || "Nothing to upload"}.` : `${planTitle}. ${buttonLabel || "Nothing to copy"}.`,
        ),
      600,
    );
    return () => clearTimeout(t);
  }, [phase, planTitle, buttonLabel]); // eslint-disable-line react-hooks/exhaustive-deps

  const handleClose = () => {
    // Closing while sending is Stop: nothing has reached the device yet, so
    // there is nothing to lose (the scrim alone stays inert, against a stray
    // tap). Like Progress, a running row action ends only by its Stop. A send
    // that failed can always be closed, whatever was waiting on it.
    if (sending) {
      stopSending();
      return;
    }
    if (!sendFailed && (phase === "progress" || rowRun.running !== null)) return;
    ctrlRef.current?.abort();
    onClose();
  };

  /** An upload's Overwrite asks first when the file has no archived copy. */
  const uploadConflict = file ? plan?.conflicts.find((c) => c.kind === "file") : undefined;
  /** Upload is disabled because the conflict's choice is Skip: say so. */
  const skipChosen = !!uploadConflict && !buttonLabel;
  const needsConfirm =
    overwriting &&
    !!uploadConflict &&
    overwriteNeedsConfirm({ code: "chose_skip", dst_ledger: uploadConflict.dst_ledger });

  let body: ReactNode = null;
  let footer: ReactNode = null;
  if (sending && file) {
    body = (
      <ProgressView
        label="Sending progress"
        note={sendingNote(to.rigName)}
        counter={sendingText(file.name)}
        bytes={`${formatBytes(sending.loaded)} of ${formatBytes(sending.total)}`}
        pct={pct(sending.loaded, sending.total)}
        current=""
        waiting={null}
      />
    );
    footer = (
      <Button id={stopId} variant="danger" onClick={stopSending}>
        Stop
      </Button>
    );
  } else if (sendFailed && file) {
    body = (
      <p className="notice tone-problem" role="alert">
        <Icon name="problem" size={18} />
        <span>{sendFailedText(file.name, to.rigName)}</span>
      </p>
    );
    footer = (
      <>
        <Button variant="ghost" onClick={handleClose}>
          Close
        </Button>
        <Button variant="primary" onClick={send}>
          Send again
        </Button>
      </>
    );
  } else if (phase === "checking") {
    body = (
      <CheckingLine
        text={
          file
            ? uploadCheckingText(file.name, to.rigName, to.share, to.dir)
            : `Checking ${from!.names.length} selected item${from!.names.length === 1 ? "" : "s"} on ${from!.rigName}…`
        }
        elapsed={elapsed}
      />
    );
    footer = (
      <Button variant="ghost" onClick={handleClose}>
        Cancel
      </Button>
    );
  } else if (phase === "planFailed") {
    body = (
      <p className="notice tone-problem" role="alert">
        <Icon name="problem" size={18} />
        <span>{planError}</span>
      </p>
    );
    footer = (
      <>
        <Button variant={planRetry ? "ghost" : "secondary"} onClick={handleClose}>
          Close
        </Button>
        {planRetry && (
          <Button variant="primary" onClick={() => void check()}>
            Try again
          </Button>
        )}
      </>
    );
  } else if ((phase === "plan" || phase === "progress") && plan && sel) {
    if (phase === "plan") {
      body = file ? (
        <UploadPlanBody
          plan={plan}
          sel={sel}
          name={uploadRel}
          decision={decisions[uploadRel] ?? "skip"}
          dispatch={dispatch}
          to={to}
          runError={runError}
          confirmText={
            confirming && uploadConflict
              ? overwriteConfirmText(
                  { code: "chose_skip", dst_ledger: uploadConflict.dst_ledger, name: uploadRel, rig: to.rigName },
                  !!itemByRel.get(uploadRel)?.watched,
                )
              : null
          }
          onConfirm={() => void start()}
          onCancelConfirm={() => setConfirming(false)}
          skipReasonId={skipChosen ? skipReasonId : null}
        />
      ) : (
        <CopyPlanBody
          plan={plan}
          sel={sel}
          decisions={decisions}
          dispatch={dispatch}
          to={to}
          runError={runError}
        />
      );
      footer = (
        <>
          <Button variant="ghost" onClick={handleClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!buttonLabel || confirming}
            aria-describedby={skipChosen ? skipReasonId : undefined}
            onClick={() => (needsConfirm ? setConfirming(true) : void start())}
          >
            {buttonLabel || (file ? "Upload" : "Nothing to copy")}
          </Button>
        </>
      );
    } else {
      body = file
        ? runProgressView("upload", progress, { total: 1, bytes: uploadSize }, waitText, {
            name: uploadRel,
            rigName: to.rigName,
          })
        : runProgressView("copy", progress, { total: sel.files, bytes: sel.bytes }, waitText);
      footer = (
        <Button id={stopId} variant="danger" onClick={stop}>
          Stop
        </Button>
      );
    }
  } else if (phase === "report") {
    const runningBytes = [...(rowRun.running ?? [])].reduce(
      (t, rel) => t + (itemByRel.get(rel)?.size ?? 0),
      0,
    );
    body = (
      <ReportView
        op={op}
        rows={rows}
        listed={listed}
        rigFor={(r) =>
          !from
            ? to.rigName
            : r.code === "not_attempted"
              ? abortRig
              : r.code && DEVICE_FAULTS.has(r.code)
                ? rigNamedIn(r.message, [from.rigName, to.rigName], to.rigName)
                : to.rigName
        }
        sourceRig={from?.rigName}
        overwriting={(r) => overwritten.current.has(r.rel)}
        runningRels={rowRun.running}
        progress={
          rowRun.running &&
          runProgressView(
            op,
            rowRun.progress,
            { total: rowRun.total, bytes: runningBytes },
            waitText,
            file ? { name: uploadRel, rigName: to.rigName } : undefined,
          )
        }
        waiting={waitText}
        error={rowRun.error}
        confirmRel={confirmRel}
        confirmText={(r) =>
          overwriteConfirmText(
            { code: r.code!, dst_ledger: r.dst_ledger, name: baseName(r.rel), rig: to.rigName },
            !!itemByRel.get(r.rel)?.watched,
          )
        }
        onOverwrite={(r) => {
          if (overwriteNeedsConfirm({ code: r.code!, dst_ledger: r.dst_ledger })) {
            setConfirmRel(r.rel);
          } else runRows([r], r);
        }}
        onConfirm={(r) => runRows([r], r)}
        onCancelConfirm={() => setConfirmRel(null)}
        onRetry={(target) => runRows(target)}
        counts={counts}
        doneLabel={file ? "uploaded" : "copied"}
      />
    );
    footer =
      rowRun.running !== null ? (
        <Button id={stopId} variant="danger" onClick={stop}>
          Stop
        </Button>
      ) : (
        <Button id={closeId} variant="secondary" onClick={handleClose}>
          Close
        </Button>
      );
  }

  return (
    <Dialog
      open
      size="lg"
      title={title}
      titleRef={titleRef}
      onClose={handleClose}
      dismissOnScrim={phase !== "progress" && phase !== "report" && !sending}
      footer={footer}
    >
      {/* The report's heading is read when focus lands on it; this region says
          only what changed without moving focus: new plan totals, and what a
          row action did together with the heading it produced. */}
      <p className="sr-only" aria-live="polite">
        {phase === "report" ? rowRun.live : phase === "plan" ? planLive : ""}
      </p>
      {body}
    </Dialog>
  );
}

function CopyPlanBody({
  plan,
  sel,
  decisions,
  dispatch,
  to,
  runError,
}: {
  plan: CopyPlan;
  sel: ReturnType<typeof copySelection>;
  decisions: Decisions;
  dispatch: (a: DecisionAction) => void;
  to: CopyTo;
  runError: string | null;
}) {
  const uid = useId();
  const fileConflicts = plan.conflicts.filter((c) => c.kind === "file");
  const hintId = (rel: string) => `${uid}-hint-${fileConflicts.findIndex((c) => c.rel === rel)}`;
  const watchedLines = copyWatchedLines(plan.watched, to.rigName, sel.skipped > 0);
  const destPath = [to.rigName, to.share, ...pathSegments(to.dir).map((s) => s.name)].join(" › ");

  const columns: Column<(typeof fileConflicts)[number]>[] = [
    {
      key: "file",
      header: "File",
      cell: (c) => {
        const hint = conflictHint(c.dst_ledger, to.rigName);
        return (
          <div className="stack dfb-conflict__file">
            <span className="mono text-sm break-anywhere">{c.rel}</span>
            {hint && (
              <span className="text-xs dfb-hint" id={hintId(c.rel)}>
                {hint}
              </span>
            )}
          </div>
        );
      },
    },
    {
      key: "src",
      header: "Copying",
      cell: (c) => (
        <span className="mono text-sm">
          <span className="dfb-mobile-label">Copying: </span>
          {formatBytes(c.src_size)}, {absoluteTime(c.src_mtime)}
        </span>
      ),
    },
    {
      key: "dst",
      header: "Existing",
      cell: (c) => (
        <span className="mono text-sm">
          <span className="dfb-mobile-label">Existing: </span>
          {formatBytes(c.dst_size)}, {absoluteTime(c.dst_mtime)}
        </span>
      ),
    },
    {
      key: "decision",
      header: "Decision",
      cell: (c) => (
        <div className="dfb-segmented" role="radiogroup" aria-label={`${c.rel}: overwrite or skip`}>
          {(["overwrite", "skip"] as const).map((d) => (
            <label key={d} className="dfb-segmented__opt">
              <input
                type="radio"
                name={`decision-${c.rel}`}
                // What overwriting this file costs is said beside it; a screen
                // reader hears it on the choice it qualifies.
                aria-describedby={
                  d === "overwrite" && conflictHint(c.dst_ledger, to.rigName) ? hintId(c.rel) : undefined
                }
                checked={(decisions[c.rel] ?? "skip") === d}
                onChange={() => dispatch({ type: "set", rel: c.rel, decision: d })}
              />
              <span>{d === "overwrite" ? "Overwrite" : "Skip"}</span>
            </label>
          ))}
        </div>
      ),
    },
  ];

  return (
    <div className="stack dfb-plan">
      {runError && (
        <p className="notice tone-problem" role="alert">
          <Icon name="problem" size={18} />
          <span>{runError}</span>
        </p>
      )}
      <div>
        <p className="text-sm text-muted">Destination</p>
        <p className="mono text-sm break-anywhere">{destPath}</p>
        {sel.createFolders > 0 && (
          <p className="text-sm">
            {sel.createFolders} folder{sel.createFolders === 1 ? "" : "s"} will be created
          </p>
        )}
      </div>

      {watchedLines.length > 0 && (
        <div className="notice tone-problem">
          <Icon name="problem" size={18} />
          <div className="notice__body">
            {watchedLines.map((l) => (
              <p key={l}>{l}</p>
            ))}
          </div>
        </div>
      )}

      {fileConflicts.length > 0 && (
        <div className="stack">
          <p>
            {fileConflicts.length} file{fileConflicts.length === 1 ? "" : "s"} already exist
            {fileConflicts.length === 1 ? "s" : ""} on {to.rigName}
          </p>
          <div className="cluster">
            <Button size="sm" variant="ghost" onClick={() => dispatch({ type: "all", decision: "overwrite" })}>
              Overwrite all
            </Button>
            <Button size="sm" variant="ghost" onClick={() => dispatch({ type: "all", decision: "skip" })}>
              Skip all
            </Button>
          </div>
          <Table
            caption="Files that already exist at the destination"
            columns={columns}
            rows={fileConflicts}
            getKey={(c) => c.rel}
            className="dfb-stack-table"
          />
        </div>
      )}

      {sel.typeConflicts.length > 0 && (
        <div className="stack">
          <p>Will be skipped: a file and a folder cannot replace each other</p>
          <ul className="list-reset">
            {sel.typeConflicts.map((t) => (
              <li key={t.rel} className="mono text-sm break-anywhere">
                {t.isDir ? `${t.rel} (${t.files} file${t.files === 1 ? "" : "s"})` : t.rel}
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

/** The upload plan for its one file: the destination, the watched warning only
 * for a watched folder, the space warning, and the file's conflict if any. */
function UploadPlanBody({
  plan,
  sel,
  name,
  decision,
  dispatch,
  to,
  runError,
  confirmText,
  onConfirm,
  onCancelConfirm,
  skipReasonId,
}: {
  plan: UploadPlan;
  sel: ReturnType<typeof copySelection>;
  name: string;
  decision: Decision;
  dispatch: (a: DecisionAction) => void;
  to: CopyTo;
  runError: string | null;
  /** The inline confirmation's question while it shows, else null. */
  confirmText: string | null;
  onConfirm: () => void;
  onCancelConfirm: () => void;
  /** The id for the line saying Skip leaves nothing to upload, while it shows. */
  skipReasonId: string | null;
}) {
  const uid = useId();
  const conflict = plan.conflicts.find((c) => c.kind === "file");
  const size = plan.items[0]?.size ?? 0;
  const watchedLines = copyWatchedLines(plan.watched, to.rigName, false);
  const space = uploadSpaceWarning(size, plan.free_bytes, to.share, to.rigName);
  const destPath = [to.rigName, to.share, ...pathSegments(to.dir).map((s) => s.name)].join(" › ");
  const hint = conflict ? conflictHint(conflict.dst_ledger, to.rigName) : null;
  const hintId = `${uid}-hint`;
  const questionId = `${uid}-q`;
  const confirmId = `${uid}-confirm`;
  useEffect(() => {
    if (confirmText) document.getElementById(confirmId)?.focus();
  }, [confirmText, confirmId]);

  return (
    <div className="stack dfb-plan">
      {runError && (
        <p className="notice tone-problem" role="alert">
          <Icon name="problem" size={18} />
          <span>{runError}</span>
        </p>
      )}
      <div>
        <p className="text-sm text-muted">Destination</p>
        <p className="mono text-sm break-anywhere">{destPath}</p>
      </div>

      {watchedLines.length > 0 && (
        <div className="notice tone-problem">
          <Icon name="problem" size={18} />
          <div className="notice__body">
            {watchedLines.map((l) => (
              <p key={l}>{l}</p>
            ))}
          </div>
        </div>
      )}

      {space && (
        <p className="notice tone-problem">
          <Icon name="problem" size={18} />
          <span>{space}</span>
        </p>
      )}

      {conflict && (
        <div className="stack">
          <p className="break-anywhere">
            {name} already exists on {to.rigName}
          </p>
          <ul className="list-reset mono text-sm">
            <li>
              Uploading: {formatBytes(conflict.src_size)}, {absoluteTime(conflict.src_mtime)}
            </li>
            <li>
              Existing: {formatBytes(conflict.dst_size)}, {absoluteTime(conflict.dst_mtime)}
            </li>
          </ul>
          {hint && (
            <p className="text-xs dfb-hint" id={hintId}>
              {hint}
            </p>
          )}
          <div className="dfb-segmented" role="radiogroup" aria-label={`${name}: overwrite or skip`}>
            {(["overwrite", "skip"] as const).map((d) => (
              <label key={d} className="dfb-segmented__opt">
                <input
                  type="radio"
                  name={`${uid}-decision`}
                  aria-describedby={d === "overwrite" && hint ? hintId : undefined}
                  checked={decision === d}
                  onChange={() => dispatch({ type: "set", rel: conflict.rel, decision: d })}
                />
                <span>{d === "overwrite" ? "Overwrite" : "Skip"}</span>
              </label>
            ))}
          </div>
          {skipReasonId && (
            <p className="text-sm text-muted" id={skipReasonId}>
              {uploadSkipReason(to.rigName)}
            </p>
          )}
          {confirmText && (
            <div className="stack dfb-confirm">
              <p className="text-sm" id={questionId}>
                {confirmText}
              </p>
              <span className="cluster">
                <Button id={confirmId} size="sm" variant="danger" aria-describedby={questionId} onClick={onConfirm}>
                  Overwrite
                </Button>
                <Button size="sm" variant="ghost" onClick={onCancelConfirm}>
                  Cancel
                </Button>
              </span>
            </div>
          )}
        </div>
      )}

      {sel.typeConflicts.length > 0 && <p>{uploadTypeConflictText(name)}</p>}
    </div>
  );
}

/* ---- delete --------------------------------------------------------------- */

export function DeleteDialog({
  rig,
  rigName,
  address,
  share,
  dir,
  names,
  onClose,
  onRunning,
  onRelist,
}: {
  rig: string;
  rigName: string;
  address?: string;
  share: string;
  dir: string;
  names: string[];
  onClose: () => void;
  onRunning: (running: boolean) => void;
  onRelist: () => void;
}) {
  const toast = useToast();
  const stopId = useId();
  const closeId = useId();
  const [phase, setPhase] = useState<Phase>("checking");
  const titleRef = usePhaseFocus(phase, stopId);
  const [plan, setPlan] = useState<DeletePlan | null>(null);
  const [planError, setPlanError] = useState<string | null>(null);
  const [runError, setRunError] = useState<string | null>(null);
  const [checkStart, setCheckStart] = useState<number | null>(null);
  const elapsed = useElapsed(checkStart);
  /** The unarchived count the user is confirming; a 409 replaces it. */
  const [unarchived, setUnarchived] = useState(0);
  const [grew, setGrew] = useState<{ now: number; was: number } | null>(null);
  const [ticked, setTicked] = useState(false);
  const [progress, setProgress] = useState<FilesProgressLine | null>(null);
  const [seq, setSeq] = useState(0);
  const [rows, setRows] = useState<ReportRow[]>([]);
  const [listed, setListed] = useState<string[]>([]);
  const [end, setEnd] = useState<RunEnd>("summary");
  const [aborted, setAborted] = useState<FilesSummaryLine["aborted"]>("");
  const ctrlRef = useRef<AbortController | null>(null);
  const stoppedRef = useRef(false);
  const checkboxId = useId();
  const tickWhyId = useId();

  useEffect(() => () => ctrlRef.current?.abort(), []);

  const check = useCallback(async () => {
    const ctrl = new AbortController();
    ctrlRef.current = ctrl;
    setPhase("checking");
    setPlanError(null);
    setCheckStart(Date.now());
    try {
      const p = await planDeviceDelete(rig, share, dir, names, ctrl.signal);
      setPlan(p);
      setUnarchived(p.unarchived);
      setTicked(false);
      setGrew(null);
      setPhase("plan");
    } catch (err) {
      if (isAbort(err)) return;
      setPlanError(errText(err));
      setPhase("planFailed");
    } finally {
      setCheckStart(null);
    }
  }, [rig, share, dir, names]);

  useEffect(() => {
    void check();
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const itemByRel = useMemo(
    () => new Map((plan?.items ?? []).map((it) => [it.rel, it])),
    [plan],
  );
  const dirSet = useMemo(() => new Set(plan?.dirs_deepest_first ?? []), [plan]);

  const onLineProgress = (p: FilesProgressLine | null) => {
    setSeq((n) => n + 1);
    if (p) setProgress(p);
  };

  /** Runs the given rels (files and folders, in run order). */
  const execute = (
    rels: string[],
    ctrl: AbortController,
    onProgress: (p: FilesProgressLine | null) => void,
    onSettled?: (rows: ReportRow[]) => void,
  ) =>
    executeRun(
      (onLine, signal) =>
        runDeviceDelete(
          rig,
          {
            share,
            dir,
            items: rels.filter((r) => itemByRel.has(r)).map((r) => itemByRel.get(r)!),
            dirs: rels.filter((r) => dirSet.has(r)),
            acknowledged_unarchived: unarchived,
          },
          onLine,
          signal,
        ),
      rels,
      ctrl,
      () => stoppedRef.current,
      onProgress,
      onSettled,
    );

  const start = async () => {
    if (!plan) return;
    const ctrl = new AbortController();
    ctrlRef.current = ctrl;
    stoppedRef.current = false;
    setRunError(null);
    setProgress(null);
    setPhase("progress");
    onRunning(true);
    const order = [...plan.items.map((i) => i.rel), ...plan.dirs_deepest_first];
    let result: RunResult;
    try {
      result = await execute(order, ctrl, onLineProgress);
    } catch (err) {
      onRunning(false);
      if (err instanceof ApiError && err.code === "unarchived") {
        // The ledger moved since the confirmation: ask again with the new number.
        const fresh = Number(err.body?.unarchived);
        const now = Number.isFinite(fresh) ? fresh : unarchived;
        setGrew({ now, was: unarchived });
        setUnarchived(now);
        setTicked(false);
      } else {
        setRunError(errText(err));
      }
      setPhase("plan");
      return;
    }
    onRunning(false);
    onRelist();
    const listedRels = result.rows.filter(isListed).map((r) => r.rel);
    if (result.end === "summary" && listedRels.length === 0) {
      toast.success(deleteDoneText(plan, rigName));
      onClose();
      return;
    }
    setRows(result.rows);
    setListed(listedRels);
    setEnd(result.end);
    setAborted(result.summary?.aborted ?? "");
    setPhase("report");
  };

  const stop = () => {
    stoppedRef.current = true;
    ctrlRef.current?.abort();
  };

  const counts = reportCounts(rows);
  const total = rows.length;
  const reportTitle =
    end === "stopped"
      ? "Delete stopped"
      : end === "interrupted"
        ? "Delete interrupted — lost connection to Commander"
        : `Deleted ${counts.done} of ${total} item${total === 1 ? "" : "s"} from ${rigName}` +
          abortedSuffix(aborted, rigName);

  const rowRun = useRowRun({
    op: "delete",
    rows,
    heading: reportTitle,
    setRows,
    setEnd,
    setAborted,
    onRunning,
    onRelist,
    stopId,
    closeId,
  });

  const waiting = useWaiting(phase === "progress" || rowRun.running !== null, seq + rowRun.seq);
  const waitText = waiting ? waitingText([rigName]) : null;

  const runRows = (target: ReportRow[]) => {
    const rels = target.map((r) => r.rel);
    // Run order: files first, then folders deepest first, as the plan had them.
    const order = [
      ...rels.filter((r) => itemByRel.has(r)),
      ...(plan?.dirs_deepest_first ?? []).filter((d) => rels.includes(d)),
    ];
    const ctrl = new AbortController();
    ctrlRef.current = ctrl;
    stoppedRef.current = false;
    void rowRun.run(order, (onProgress, onSettled) => execute(order, ctrl, onProgress, onSettled));
  };

  const handleClose = () => {
    // Like Progress, a running row action ends only by its Stop.
    if (phase === "progress" || rowRun.running !== null) return;
    ctrlRef.current?.abort();
    onClose();
  };

  let body: ReactNode = null;
  let footer: ReactNode = null;
  if (phase === "checking") {
    body = (
      <CheckingLine
        text={`Checking ${names.length} selected item${names.length === 1 ? "" : "s"} on ${rigName}…`}
        elapsed={elapsed}
      />
    );
    footer = (
      <Button variant="ghost" onClick={handleClose}>
        Cancel
      </Button>
    );
  } else if (phase === "planFailed") {
    body = (
      <p className="notice tone-problem" role="alert">
        <Icon name="problem" size={18} />
        <span>Couldn't check the selection on {rigName}: {planError}</span>
      </p>
    );
    footer = (
      <>
        <Button variant="ghost" onClick={handleClose}>
          Close
        </Button>
        <Button variant="primary" onClick={() => void check()}>
          Try again
        </Button>
      </>
    );
  } else if (phase === "plan" && plan) {
    const inWatched = plan.files - plan.unwatched_files;
    const needsTick = unarchived > 0 && !ticked;
    body = (
      <div className="stack dfb-plan">
        <p className="text-sm text-muted break-anywhere">
          {deleteSelectionLine(names, rigName, share, dir)}
        </p>
        {runError && (
          <p className="notice tone-problem" role="alert">
            <Icon name="problem" size={18} />
            <span>{runError}</span>
          </p>
        )}
        {grew && (
          <p className="notice tone-problem" role="alert">
            <Icon name="problem" size={18} />
            <span>{unarchivedGrewNotice(grew.now, grew.was)}</span>
          </p>
        )}
        <p>{deleteSummaryLine(plan)}</p>
        {plan.by_ext.length > 0 && (
          <ul className="list-reset mono text-sm">
            {extSummaryLines(plan.by_ext).map((l) => (
              <li key={l}>{l}</li>
            ))}
          </ul>
        )}
        {unarchived > 0 ? (
          <div className="stack">
            <p className="notice tone-problem">
              <Icon name="problem" size={18} />
              <span>{unarchivedNotice(unarchived)}</span>
            </p>
            <label className="cluster" htmlFor={checkboxId}>
              <input
                id={checkboxId}
                type="checkbox"
                className="aac-checkbox"
                checked={ticked}
                onChange={(e) => setTicked(e.target.checked)}
              />
              <span>{unarchivedCheckboxLabel(unarchived)}</span>
            </label>
          </div>
        ) : (
          inWatched > 0 && (
            <p className="text-sm text-muted">
              Every frame in watched folders is archived in your library.
            </p>
          )
        )}
        {plan.unwatched_files > 0 && (
          <p className="text-sm">
            {plan.unwatched_files} file{plan.unwatched_files === 1 ? " is" : "s are"} outside
            folders Commander watches; Commander keeps no copy of{" "}
            {plan.unwatched_files === 1 ? "it" : "them"}.
          </p>
        )}
        {plan.watched_roots.map((root) => (
          <p key={root} className="notice tone-problem">
            <Icon name="problem" size={18} />
            <span>{watchedRootLine(baseName(root), share, rigName)}</span>
          </p>
        ))}
        {needsTick && (
          <span id={tickWhyId} className="sr-only">
            Tick the box above to enable
          </span>
        )}
      </div>
    );
    footer = (
      <>
        <Button variant="ghost" onClick={handleClose}>
          Cancel
        </Button>
        <Button
          variant="danger"
          disabled={needsTick || plan.files + plan.dirs === 0}
          aria-describedby={needsTick ? tickWhyId : undefined}
          onClick={() => void start()}
        >
          {deleteButtonLabel(plan)}
        </Button>
      </>
    );
  } else if (phase === "progress" && plan) {
    body = runProgressView(
      "delete",
      progress,
      { total: plan.items.length + plan.dirs_deepest_first.length, bytes: 0 },
      waitText,
    );
    footer = (
      <Button id={stopId} variant="danger" onClick={stop}>
        Stop
      </Button>
    );
  } else if (phase === "report") {
    body = (
      <ReportView
        op="delete"
        rows={rows}
        listed={listed}
        rigFor={() => rigName}
        address={address}
        runningRels={rowRun.running}
        progress={
          rowRun.running &&
          runProgressView("delete", rowRun.progress, { total: rowRun.total, bytes: 0 }, waitText)
        }
        waiting={waitText}
        error={rowRun.error}
        confirmRel={null}
        confirmText={() => ""}
        onOverwrite={() => {}}
        onConfirm={() => {}}
        onCancelConfirm={() => {}}
        onRetry={runRows}
        counts={counts}
        doneLabel="deleted"
      />
    );
    footer =
      rowRun.running !== null ? (
        <Button id={stopId} variant="danger" onClick={stop}>
          Stop
        </Button>
      ) : (
        <Button id={closeId} variant="secondary" onClick={handleClose}>
          Close
        </Button>
      );
  }

  const title = phase === "report" ? reportTitle : `Delete from ${rigName}?`;

  return (
    <Dialog
      open
      size="lg"
      title={title}
      titleRef={titleRef}
      onClose={handleClose}
      dismissOnScrim={phase !== "progress" && phase !== "report"}
      footer={footer}
    >
      {/* What a row action did, with the heading it produced; the heading itself
          is read when focus lands on it as the report opens. */}
      <p className="sr-only" aria-live="polite">
        {phase === "report" ? rowRun.live : ""}
      </p>
      {body}
    </Dialog>
  );
}
