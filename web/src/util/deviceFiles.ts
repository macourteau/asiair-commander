/**
 * Pure logic behind the File browser page (design
 * device-file-browser §3, and
 * design device-file-browser §6): the toolbar's enable rules, the
 * name rules, the copy plan's decision reducer and the counts derived from it,
 * the delete summary, the upload and rename wording, the report wording and the
 * client-built report of a stopped or interrupted run. Kept out of the surface
 * so each rule has a vitest.
 *
 * Paths are share-relative and "/"-separated, with "" for the share root, the
 * server's convention. ASIAIR media are FAT, so path comparisons fold case.
 */
import type {
  CopyConflict,
  CopyOverwrite,
  CopyPlan,
  DeletePlan,
  DstLedger,
  ExtTotal,
  FileRenameRefusal,
  FilesItemLine,
  FilesReasonCode,
  FilesSummaryLine,
  RigProfile,
  RigStatus,
} from "../api/types";
import { formatBytes } from "./format";

export type Side = "left" | "right";

const plural = (n: number, one: string, many = `${one}s`) =>
  `${n} ${n === 1 ? one : many}`;

/* ---- paths ---------------------------------------------------------------- */

export function joinPath(dir: string, name: string): string {
  if (!dir) return name;
  if (!name) return dir;
  return `${dir}/${name}`;
}

export function parentDir(path: string): string {
  const i = path.lastIndexOf("/");
  return i < 0 ? "" : path.slice(0, i);
}

export function baseName(path: string): string {
  const i = path.lastIndexOf("/");
  return i < 0 ? path : path.slice(i + 1);
}

/** The breadcrumb's folder segments of `dir`, each with the path it opens. */
export function pathSegments(dir: string): { name: string; path: string }[] {
  if (!dir) return [];
  const parts = dir.split("/");
  return parts.map((name, i) => ({ name, path: parts.slice(0, i + 1).join("/") }));
}

export function samePath(a: string, b: string): boolean {
  return a.toLowerCase() === b.toLowerCase();
}

/** `path` is `ancestor` or lies inside it. */
export function isSameOrInside(path: string, ancestor: string): boolean {
  if (samePath(path, ancestor)) return true;
  if (!ancestor) return true;
  return path.toLowerCase().startsWith(`${ancestor.toLowerCase()}/`);
}

/** Every ancestor folder of `rel`, nearest the root first ("a/b/c" → a, a/b). */
export function ancestorsOf(rel: string): string[] {
  const out: string[] = [];
  let p = parentDir(rel);
  while (p) {
    out.unshift(p);
    p = parentDir(p);
  }
  return out;
}

/* ---- pane place in the query string --------------------------------------- */

/** Where a pane is: no rig, a rig's share list (share null), or a folder. */
export interface PanePlace {
  rig: string | null;
  share: string | null;
  dir: string;
}

const KEYS: Record<Side, { rig: string; share: string; dir: string }> = {
  left: { rig: "lrig", share: "lshare", dir: "ldir" },
  right: { rig: "rrig", share: "rshare", dir: "rdir" },
};

/**
 * A pane's place from the query string, or null when the URL does not name its
 * rig at all (so the default applies). An empty `lrig=` is an explicit "no rig".
 */
export function readPanePlace(params: URLSearchParams, side: Side): PanePlace | null {
  const k = KEYS[side];
  if (!params.has(k.rig)) return null;
  const rig = params.get(k.rig) || null;
  if (!rig) return { rig: null, share: null, dir: "" };
  const share = params.has(k.share) ? params.get(k.share)! : null;
  return { rig, share, dir: share === null ? "" : params.get(k.dir) ?? "" };
}

export function writePanePlace(
  params: URLSearchParams,
  side: Side,
  place: PanePlace,
): URLSearchParams {
  const k = KEYS[side];
  const next = new URLSearchParams(params);
  next.set(k.rig, place.rig ?? "");
  next.delete(k.share);
  next.delete(k.dir);
  if (place.rig && place.share !== null) {
    next.set(k.share, place.share);
    if (place.dir) next.set(k.dir, place.dir);
  }
  return next;
}

/* ---- rig selector --------------------------------------------------------- */

export interface RigOption {
  value: string;
  label: string;
  disabled: boolean;
}

/** The selector's rigs: every managed rig the snapshot carries (the hub drops a
 * rig when it leaves the managed set), offline ones disabled. */
export function rigOptions(statuses: RigStatus[]): RigOption[] {
  return statuses.map((s) => {
    const name = s.name || s.id;
    if (!s.online) return { value: s.id, label: `${name} (offline)`, disabled: true };
    const remote = s.remote_name || s.site;
    return {
      value: s.id,
      label: remote ? `${name} (via ${remote})` : name,
      disabled: false,
    };
  });
}

/** Left gets the first online rig, Right the second, or none. */
export function defaultRigs(statuses: RigStatus[]): [string | null, string | null] {
  const online = statuses.filter((s) => s.online);
  return [online[0]?.id ?? null, online[1]?.id ?? null];
}

/**
 * Why a pane's selected rig can't be used right now, or null. A pane keeps its
 * rig when it goes offline or leaves the managed set rather than clearing it.
 * `profiles` null means the configured rigs are not loaded yet.
 */
export function paneRigNotice(
  rigId: string,
  statuses: RigStatus[],
  profiles: RigProfile[] | null,
): string | null {
  const st = statuses.find((s) => s.id === rigId);
  if (st) return st.online ? null : `${st.name || rigId} is offline`;
  if (profiles === null) return null;
  const profile = profiles.find((p) => p.id === rigId);
  // Most often a hand-typed or stale URL rather than a rig that was removed.
  if (!profile) return `${rigId} is not one of your managed rigs`;
  if (!profile.managed) return `${profile.name || rigId} is no longer managed`;
  // Managed but not yet published by its worker: nothing to say yet.
  return null;
}

/** The rig is neither in the snapshot nor configured at all: the pane treats it
 * as no rig (nothing to list, retry or identify). False until the configured
 * rigs are loaded. */
export function isUnknownRig(
  rigId: string,
  statuses: RigStatus[],
  profiles: RigProfile[] | null,
): boolean {
  if (profiles === null || statuses.some((s) => s.id === rigId)) return false;
  return !profiles.some((p) => p.id === rigId);
}

/** The pane listing's accessible name: "Left pane: Alpha › EMMC Images › Preview". */
export function listingRegionLabel(
  side: Side,
  rigName: string,
  share: string | null,
  dir: string,
): string {
  const where = [rigName, ...(share === null ? [] : [share]), ...pathSegments(dir).map((s) => s.name)]
    .filter(Boolean)
    .join(" › ");
  const pane = side === "left" ? "Left pane" : "Right pane";
  return where ? `${pane}: ${where}` : pane;
}

/* ---- toolbar -------------------------------------------------------------- */

/** What toolbarState needs to know about one pane. */
export interface ToolbarPane {
  rig: string | null;
  rigName: string;
  share: string | null;
  dir: string;
  /** From the rig's latest listing; null until one has answered. */
  writable: boolean | null;
  /** The pane's folder answered not_found. */
  notFound: boolean;
  /** The listing's folder is watched by an enabled source (its notice shows). */
  watched?: boolean;
  selected: { name: string; dir: boolean; renameRefused?: FileRenameRefusal }[];
  /** The rig's share names, once listed. */
  shares: string[] | null;
}

export interface ButtonState {
  enabled: boolean;
  /** Why it is disabled, for its title and aria-describedby. */
  reason?: string;
}

export interface PaneToolbar {
  download: ButtonState;
  copyOther: ButtonState;
  copySame: ButtonState;
  upload: ButtonState;
  newFolder: ButtonState;
  rename: ButtonState;
  delete: ButtonState;
  /** The copy button's label and accessible name: "Copy to ‹rig›", or "Copy to
   * other folder" when both panes show one rig. "Left" and "right" mean nothing
   * once the panes stack on a phone. */
  copyOtherLabel: string;
  /** Pane-level reasons, shown as visible lines under the toolbar: touch devices
   * never show a title, and a disabled button cannot take focus. */
  paneReasons: string[];
  /** The one selected item's rename refusal when it has its own line, for the
   * pane's live region: a keyboard user hears why Rename is unavailable
   * without reaching the line. Null otherwise. */
  renameRefusal: string | null;
}

/** The visible line for the common empty-selection state, so a keyboard user
 * reaches the reason in reading order (a disabled button takes no focus). */
export const SELECT_FIRST_REASON =
  "Select files or folders to download, copy, rename or delete.";
/** SELECT_FIRST_REASON in a watched folder, where nothing can be renamed. */
export const SELECT_FIRST_REASON_WATCHED = "Select files or folders to download, copy or delete.";

export const IDENTIFYING_REASON =
  "Commander is still identifying this ASIAIR; writes are disabled until it has";
export const BUSY_REASON = "Another copy, upload, rename, delete or download is running.";
/** Said once in a watched folder's notice rather than on every selection there. */
export const WATCHED_RENAME_NOTE = "Items here can't be renamed.";

const on: ButtonState = { enabled: true };
/** Before the other rig's listing and share list answer, neither "no share
 * named…" nor "still identifying" is known. */
const otherLoading = (rigName: string) => `Waiting for ${rigName}'s folders to load.`;
const off = (reason: string): ButtonState => ({ enabled: false, reason });

function paneToolbar(
  self: ToolbarPane,
  other: ToolbarPane,
  side: Side,
  busy: boolean,
): PaneToolbar {
  const otherSide = side === "left" ? "right" : "left";
  const copyOtherLabel = !other.rig
    ? "Copy to other pane"
    : other.rig === self.rig
      ? "Copy to other folder"
      : `Copy to ${other.rigName}`;

  const paneReasons: string[] = [];
  if (self.rig && self.writable === false) paneReasons.push(IDENTIFYING_REASON);
  if (self.rig && !other.rig) {
    paneReasons.push(`Choose a rig in the ${otherSide} pane to copy there.`);
  }

  const n = self.selected.length;
  if (self.rig && self.share !== null && !self.notFound && n === 0 && !busy) {
    paneReasons.push(self.watched ? SELECT_FIRST_REASON_WATCHED : SELECT_FIRST_REASON);
  }
  const needSelection = (verb: string): ButtonState | null => {
    if (!self.rig) return off("Choose a rig first.");
    if (self.share === null) return off("Open a share first.");
    if (self.notFound) return off("This folder no longer exists.");
    if (n === 0) return off(`Select files or folders to ${verb}.`);
    return null;
  };

  let download: ButtonState = needSelection("download") ?? on;
  let copyOther: ButtonState = needSelection("copy") ?? on;
  let copySame: ButtonState = needSelection("copy") ?? on;
  // Upload adds to the current folder exactly as New folder does.
  let newFolder: ButtonState = !self.rig
    ? off("Choose a rig first.")
    : self.share === null
      ? off("Open a share first.")
      : self.notFound
        ? off("This folder no longer exists.")
        : self.writable !== true
          ? off(IDENTIFYING_REASON)
          : on;
  let upload: ButtonState = newFolder;
  let del: ButtonState =
    needSelection("delete") ?? (self.writable !== true ? off(IDENTIFYING_REASON) : on);

  const refusal = n === 1 ? self.selected[0]!.renameRefused : undefined;
  let rename: ButtonState = !self.rig
    ? off("Choose a rig first.")
    : self.share === null
      ? off("Open a share first.")
      : self.notFound
        ? off("This folder no longer exists.")
        : n === 0
          ? off("Select one file or folder to rename.")
          : n > 1
            ? off("Rename works on one item at a time.")
            : self.writable !== true
              ? off(IDENTIFYING_REASON)
              : refusal
                ? off(refusal.message)
                : on;
  const renameRefusal =
    refusal && !busy && renameRefusalShown(refusal, !!self.watched) ? refusal.message : null;
  if (renameRefusal) paneReasons.push(renameRefusal);

  if (copyOther.enabled) {
    if (!other.rig) copyOther = off(`Choose a rig in the ${otherSide} pane.`);
    else if (other.share === null) copyOther = off(`Open a folder in the ${otherSide} pane.`);
    else if (other.notFound) copyOther = off(`The ${otherSide} pane's folder no longer exists.`);
    else if (other.writable !== true) {
      copyOther = off(`${other.rigName}: ${IDENTIFYING_REASON}.`);
    } else if (other.rig === self.rig && samePath(other.share, self.share ?? "")) {
      if (samePath(other.dir, self.dir)) {
        copyOther = off("Both panes show the same folder.");
      } else if (
        self.selected.some(
          (s) => s.dir && isSameOrInside(other.dir, joinPath(self.dir, s.name)),
        )
      ) {
        copyOther = off("A folder can't be copied into itself.");
      }
    }
  }

  if (copySame.enabled) {
    if (!other.rig) copySame = off(`Choose a rig in the ${otherSide} pane.`);
    else if (other.rig === self.rig) {
      copySame = off("The other pane shows the same rig; choose a different one.");
    } else if (other.writable === null) {
      copySame = off(otherLoading(other.rigName));
    } else if (other.writable === false) {
      copySame = off(`${other.rigName}: ${IDENTIFYING_REASON}.`);
    } else if (other.shares === null) {
      copySame = off(otherLoading(other.rigName));
    } else if (!other.shares.includes(self.share ?? "")) {
      copySame = off(`${other.rigName} has no share named ${self.share}.`);
    }
  }

  if (busy) {
    download = off(BUSY_REASON);
    copyOther = off(BUSY_REASON);
    copySame = off(BUSY_REASON);
    upload = off(BUSY_REASON);
    newFolder = off(BUSY_REASON);
    rename = off(BUSY_REASON);
    del = off(BUSY_REASON);
  }

  return {
    download,
    copyOther,
    copySame,
    upload,
    newFolder,
    rename,
    delete: del,
    copyOtherLabel,
    paneReasons,
    renameRefusal,
  };
}

/** Every toolbar button's enable rule and disabled reason, for both panes.
 * `busy` covers the whole card: one operation runs at a time. */
export function toolbarState(
  left: ToolbarPane,
  right: ToolbarPane,
  busy: boolean,
): Record<Side, PaneToolbar> {
  return {
    left: paneToolbar(left, right, "left", busy),
    right: paneToolbar(right, left, "right", busy),
  };
}

/**
 * Whether a selected entry's rename refusal gets its own visible line under the
 * toolbar (touch devices never show a title). A refusal because the folder lies
 * in a root an enabled source watches is said once, in the folder's watched
 * notice; the client tells it from the listing's `watched` flag, which is false
 * when only a turned-off source or a root the store keeps in play caused it.
 */
export function renameRefusalShown(refusal: FileRenameRefusal, folderWatched: boolean): boolean {
  return !(refusal.rule === "watched" && folderWatched);
}

/** "3 selected · 1.1 GB"; folders carry no size. */
export function selectionText(selected: { dir: boolean; size: number }[]): string {
  if (selected.length === 0) return "";
  const bytes = selected.reduce((t, s) => t + (s.dir ? 0 : s.size), 0);
  return bytes > 0
    ? `${selected.length} selected · ${formatBytes(bytes)}`
    : `${selected.length} selected`;
}

/* ---- names ---------------------------------------------------------------- */

const FORBIDDEN = /[/\\:*?"<>|]/;
/** Unicode Cc and Cf: controls, and the invisible format characters (zero-width
 * spaces and joiners, bidi overrides and isolates, the BOM) that make a name
 * read like another. */
const CONTROL = /[\p{Cc}\p{Cf}]/u;
/** Commander's device marker files, which the tool never creates or touches. */
const MARKER_NAMES = [".asiair-commander-media-id", ".asiair-commander-rig-id"];
const NAME_MAX_BYTES = 255;
/** 255 less the 31 bytes of the temp suffix `.<16 hex>.commander.tmp`, so an
 * upload's temp name fits the device's limit. */
export const UPLOAD_NAME_MAX_BYTES = 224;

const byteLength = (name: string) => new TextEncoder().encode(name).length;

type NameRule =
  | "empty"
  | "dots"
  | "forbidden"
  | "control"
  | "leading_space"
  | "trailing"
  | "too_long"
  | "marker";

/** The first rule `name` breaks, or null. The server applies the same rules. */
function nameRule(name: string, maxBytes = NAME_MAX_BYTES): NameRule | null {
  if (name === "") return "empty";
  if (name === "." || name === "..") return "dots";
  if (FORBIDDEN.test(name)) return "forbidden";
  if (CONTROL.test(name)) return "control";
  if (name.startsWith(" ")) return "leading_space";
  if (name.endsWith(" ") || name.endsWith(".")) return "trailing";
  if (byteLength(name) > maxBytes) return "too_long";
  if (MARKER_NAMES.some((m) => samePath(m, name))) return "marker";
  return null;
}

/** What a name is for: only the wording differs. */
export type NameKind = "folder" | "file" | "any";

/** Why `name` can't be a folder's, a file's or any item's name, or null. Mirrors
 * the server, and FAT's rules: the ASIAIR's removable media are FAT. */
export function validateName(name: string, kind: NameKind): string | null {
  const Names = kind === "folder" ? "Folder names" : kind === "file" ? "File names" : "Names";
  const aName = kind === "folder" ? "a folder name" : kind === "file" ? "a file name" : "a name";
  switch (nameRule(name)) {
    case null:
      return null;
    case "empty":
      return `Enter ${aName}.`;
    case "dots":
      return `"${name}" can't be used as ${aName}.`;
    case "forbidden":
      return `${Names} can't contain / \\ : * ? " < > |.`;
    case "control":
      return `${Names} can't contain control or invisible formatting characters.`;
    case "leading_space":
      return `${Names} can't start with a space.`;
    case "trailing":
      return `${Names} can't end with a space or a dot.`;
    case "too_long":
      return `${Names} are limited to 255 bytes.`;
    case "marker":
      return `${name} is the name of Commander's device-identity file.`;
  }
}

/**
 * The Plan failed sentence for a picked file whose name the device or the
 * upload cap refuses, or null. Nothing on the device side can fix a PC-side
 * name, so the sentence says where to fix it.
 */
export function uploadPickProblem(name: string, rigName: string): string | null {
  const rule = nameRule(name, UPLOAD_NAME_MAX_BYTES);
  if (!rule) return null;
  const why: Record<NameRule, string> = {
    empty: "has no name",
    dots: "is . or ..",
    forbidden: `contains one of / \\ : * ? " < > |`,
    control: "contains a control or invisible formatting character",
    leading_space: "starts with a space",
    trailing: "ends with a space or a dot",
    too_long: `name too long for ${rigName}`,
    marker: "is Commander's device-identity file name",
  };
  return `${name} can't be uploaded under its current name (${why[rule]}). Rename it on your computer and choose it again.`;
}

/**
 * The part of a name the Rename field pre-selects, as [start, end): the stem
 * before the last dot of a file, so typing keeps the extension; the whole name
 * for a folder, a dotfile or a name without a dot.
 */
export function renameSelection(name: string, isDir: boolean): [number, number] {
  const dot = name.lastIndexOf(".");
  if (isDir || dot <= 0) return [0, name.length];
  return [0, dot];
}

/* ---- copy plan ------------------------------------------------------------ */

export type Decision = "overwrite" | "skip";
/** The user's choice per overwritable (`file`) conflict, keyed by rel. */
export type Decisions = Record<string, Decision>;

export type DecisionAction =
  | { type: "set"; rel: string; decision: Decision }
  | { type: "all"; decision: Decision };

/** Every overwritable conflict starts at Skip. */
export function initialDecisions(conflicts: CopyConflict[]): Decisions {
  const out: Decisions = {};
  for (const c of conflicts) if (c.kind === "file") out[c.rel] = "skip";
  return out;
}

/** The copy dialog's decision reducer. Only known conflicts can be set. */
export function conflictDecisions(state: Decisions, action: DecisionAction): Decisions {
  if (action.type === "all") {
    const next: Decisions = {};
    for (const rel of Object.keys(state)) next[rel] = action.decision;
    return next;
  }
  if (!(action.rel in state)) return state;
  return { ...state, [action.rel]: action.decision };
}

export interface TypeConflictRow {
  rel: string;
  /** The source is a folder (its subtree drops out), not a file. */
  isDir: boolean;
  /** Files in that subtree, for "M42 (14 files)". */
  files: number;
}

/** What a plan sends once the user's decisions are applied: every count the
 * dialog shows comes from here, so it counts what will actually be copied. */
export interface CopySelection {
  items: { rel: string; size: number }[];
  overwrite: CopyOverwrite[];
  /** Folders created inside to.dir, type-conflicted subtrees removed. */
  dirs: string[];
  files: number;
  bytes: number;
  /** Planned files that will not be sent (chosen skips and type conflicts). */
  skipped: number;
  /** Folders created in all: `dirs` plus the missing levels of to.dir. */
  createFolders: number;
  typeConflicts: TypeConflictRow[];
}

export function copySelection(plan: CopyPlan, decisions: Decisions): CopySelection {
  const typeRels = plan.conflicts.filter((c) => c.kind === "type").map((c) => c.rel);
  const inTypeConflict = (rel: string) => typeRels.some((t) => isSameOrInside(rel, t));
  const fileConflicts = new Map(
    plan.conflicts.filter((c) => c.kind === "file").map((c) => [c.rel, c]),
  );

  const items: { rel: string; size: number }[] = [];
  const overwrite: CopyOverwrite[] = [];
  let skipped = 0;
  let bytes = 0;
  for (const it of plan.items) {
    if (inTypeConflict(it.rel)) {
      skipped++;
      continue;
    }
    const conflict = fileConflicts.get(it.rel);
    if (conflict) {
      if ((decisions[it.rel] ?? "skip") === "skip") {
        skipped++;
        continue;
      }
      overwrite.push({
        rel: it.rel,
        dst_size: conflict.dst_size,
        dst_mtime: conflict.dst_mtime,
      });
    }
    items.push({ rel: it.rel, size: it.size });
    bytes += it.size;
  }

  const dirs = plan.dirs.filter((d) => !inTypeConflict(d));
  const typeConflicts = typeRels.map((rel) => {
    const under = plan.items.filter((it) => !samePath(it.rel, rel) && isSameOrInside(it.rel, rel));
    return { rel, isDir: under.length > 0, files: under.length };
  });

  return {
    items,
    overwrite,
    dirs,
    files: items.length,
    bytes,
    skipped,
    createFolders: dirs.length + plan.to_dir_missing,
    typeConflicts,
  };
}

export function copyPlanTitle(
  sel: Pick<CopySelection, "files" | "bytes" | "skipped" | "createFolders">,
  destRig: string,
  mirror: boolean,
): string {
  const where = mirror ? `the same location on ${destRig}` : destRig;
  if (sel.files === 0) {
    if (sel.skipped > 0) {
      const exist = sel.skipped === 1 ? "already exists" : "already exist";
      return `Nothing to copy: ${plural(sel.skipped, "file")} ${exist} on ${destRig}`;
    }
    if (sel.createFolders > 0) return `Create ${plural(sel.createFolders, "folder")} on ${where}`;
    return "Nothing to copy";
  }
  return `Copy ${plural(sel.files, "file")} (${formatBytes(sel.bytes)}) to ${where}`;
}

/** The toast for a copy that finished with nothing to report: a folders-only
 * copy names the folders it created, as its plan title did. */
export function copyDoneText(
  sel: Pick<CopySelection, "files" | "bytes" | "createFolders">,
  destRig: string,
): string {
  if (sel.files === 0) return `Created ${plural(sel.createFolders, "folder")} on ${destRig}`;
  return `Copied ${plural(sel.files, "file")} (${formatBytes(sel.bytes)}) to ${destRig}`;
}

/** The plan's primary button; empty when nothing is left to copy or create. */
export function copyButtonLabel(
  sel: Pick<CopySelection, "files" | "skipped" | "createFolders">,
): string {
  if (sel.files === 0) {
    return sel.createFolders > 0 ? `Create ${plural(sel.createFolders, "folder")}` : "";
  }
  let label = `Copy ${plural(sel.files, "file")}`;
  if (sel.skipped > 0) label += `, skip ${sel.skipped}`;
  if (sel.createFolders > 0) label += `, create ${plural(sel.createFolders, "folder")}`;
  return label;
}

/**
 * The watched warning's sentences, or [] when no file lands in a folder the
 * destination harvester watches: archiving is never mentioned for a copy into an
 * unwatched folder. The server's counts cover the whole plan, so once anything
 * is skipped they become an upper bound.
 */
export function copyWatchedLines(
  watched: CopyPlan["watched"],
  destRig: string,
  upTo: boolean,
): string[] {
  const pre = upTo ? "Up to " : "";
  const lines: string[] = [];
  if (watched.frames > 0) {
    const one = watched.frames === 1;
    const n = `${pre}${plural(watched.frames, "frame")}`;
    const as = one ? `one of ${destRig}'s frames` : `${destRig}'s frames`;
    lines.push(
      watched.keeps_originals
        ? `${n} will be archived as ${as} and filed under its telescope and session; ${destRig} keeps originals, so ${one ? "it stays" : "they stay"} on the device.`
        : `${n} will be archived as ${as}, filed under its telescope and session, and then removed from ${destRig} after the grace period.`,
    );
  }
  if (watched.previews > 0) {
    const n = `${pre}${plural(watched.previews, "preview")}`;
    const are = watched.previews === 1 ? "is" : "are";
    lines.push(
      watched.keeps_originals
        ? `${n} (jpg and other excluded file types) ${are} not archived and stay on ${destRig}.`
        : `${n} (jpg and other excluded file types) ${are} not archived, and Commander may remove them from ${destRig}.`,
    );
  }
  return lines;
}

/** The conflict row's hint for a destination file the harvester tracks. */
export function conflictHint(ledger: DstLedger, destRig: string): string | null {
  if (ledger === "unarchived") return "Not archived yet: overwriting loses it.";
  if (ledger === "archived") {
    return `Commander archived this frame and still tracks it: replacing it with different content raises an integrity alert on ${destRig}.`;
  }
  return null;
}

/* ---- delete plan ---------------------------------------------------------- */

/** One line per extension, largest total first: "10 fits — 1.1 GB". */
export function extSummaryLines(byExt: ExtTotal[]): string[] {
  return [...byExt]
    .sort((a, b) => b.bytes - a.bytes || a.ext.localeCompare(b.ext))
    .map((e) => `${e.files} ${e.ext || "(no extension)"} — ${formatBytes(e.bytes)}`);
}

/** "3 folders, 120 files, 1.2 GB", or "2 empty folders". */
export function deleteSummaryLine(plan: Pick<DeletePlan, "dirs" | "files" | "bytes">): string {
  if (plan.files === 0) return plural(plan.dirs, "empty folder");
  const parts = [plural(plan.files, "file"), formatBytes(plan.bytes)];
  if (plan.dirs > 0) parts.unshift(plural(plan.dirs, "folder"));
  return parts.join(", ");
}

/** "3 files and 1 folder", the delete button's and toast's object. */
function deleteObject(plan: Pick<DeletePlan, "dirs" | "files">): string {
  if (plan.files === 0) return plural(plan.dirs, "folder");
  if (plan.dirs === 0) return plural(plan.files, "file");
  return `${plural(plan.files, "file")} and ${plural(plan.dirs, "folder")}`;
}

export function deleteButtonLabel(plan: Pick<DeletePlan, "dirs" | "files">): string {
  return `Delete ${deleteObject(plan)}`;
}

/** The toast of a clean delete, worded like the button that started it. */
export function deleteDoneText(plan: Pick<DeletePlan, "dirs" | "files">, rigName: string): string {
  return `Deleted ${deleteObject(plan)} from ${rigName}`;
}

/** "'M42' and 2 other items in Rig › EMMC Images › Preview". */
export function deleteSelectionLine(
  names: string[],
  rigName: string,
  share: string,
  dir: string,
): string {
  const place = [rigName, share, ...pathSegments(dir).map((s) => s.name)].join(" › ");
  const first = `'${names[0] ?? ""}'`;
  const rest = names.length - 1;
  const who = rest > 0 ? `${first} and ${plural(rest, "other item")}` : first;
  return `${who} in ${place}`;
}

export function unarchivedNotice(n: number): string {
  return n === 1
    ? "1 file has no archived copy in your library. Deleting it removes the only copy."
    : `${n} files have no archived copy in your library. Deleting them removes the only copy.`;
}

export function unarchivedCheckboxLabel(n: number): string {
  return n === 1
    ? "I understand this file will be lost"
    : `I understand these ${n} files will be lost`;
}

export function unarchivedGrewNotice(now: number, was: number): string {
  return `Since you confirmed, more files lost their archived copy: now ${now}, was ${was}. Nothing was deleted.`;
}

export function watchedRootLine(folder: string, share: string, rigName: string): string {
  return `${folder} is a folder Commander watches. Until it exists again, nothing on ${share} is archived from ${rigName} and Commander will not report it safe to power off. Recreate it with New folder.`;
}

/** The notice over a watched folder's listing; it also says, once for the
 * folder, that its items can't be renamed. */
export function watchedFolderNotice(rigName: string, keepsOriginals: boolean): string {
  return keepsOriginals
    ? `Commander watches this folder. Frames saved here are archived to your library and left on ${rigName}. ${WATCHED_RENAME_NOTE}`
    : `Commander watches this folder. Frames saved here are archived to your library, then removed from ${rigName} after the grace period. ${WATCHED_RENAME_NOTE}`;
}

/* ---- report reasons ------------------------------------------------------- */

export type RowAction = "overwrite" | "retry" | null;
export type RunOp = "copy" | "delete" | "upload";
/** Why a not_attempted item was not attempted: `identity` is a run the server
 * ended on a wrong device or a rig not yet identified. */
export type NotAttemptedCause = "unreachable" | "identity" | "stopped" | "lost";

export interface ReasonContext {
  op: RunOp;
  /** The rig the item was written to or deleted from. */
  rig: string;
  /** A copy's source rig, for source_gone. Defaults to `rig`. */
  sourceRig?: string;
  /** The item's file name. */
  name: string;
  /** The address the device answered at, for identity. */
  address?: string;
  /** The server's message, shown for `error`. */
  message?: string;
  cause?: NotAttemptedCause;
  /** An upload's run was bound to overwrite the destination. */
  overwriting?: boolean;
}

/** The report's wording and action for each reason code. */
export function reasonText(
  code: FilesReasonCode,
  ctx: ReasonContext,
): { text: string; action: RowAction } {
  const { rig, name } = ctx;
  switch (code) {
    case "chose_skip":
      return { text: "Already exists: you chose Skip.", action: "overwrite" };
    case "appeared":
      return {
        text: `Appeared on ${rig} after the check, so it was not overwritten.`,
        action: "overwrite",
      };
    case "type_conflict":
      return { text: "A file and a folder cannot replace each other.", action: null };
    case "source_gone":
      return { text: `No longer on ${ctx.sourceRig ?? rig}.`, action: null };
    case "changed":
      return ctx.op === "delete"
        ? { text: `Changed on ${rig} since you confirmed, so it was not deleted.`, action: null }
        : {
            text: `Changed on ${rig} since you confirmed, so it was not overwritten.`,
            action: "overwrite",
          };
    case "already_gone":
      return { text: "Already gone.", action: null };
    case "not_empty":
      return {
        text: `Folder not removed: ${rig} reports it still contains files.`,
        action: null,
      };
    case "kept_marker":
      return { text: "Kept: it holds Commander's device-identity file.", action: null };
    case "denied":
      return { text: `${rig} refused: access denied.`, action: "retry" };
    case "in_use":
      return {
        text: `${name} is in use on ${rig} (the ASIAIR or Commander may be reading or writing it).`,
        action: "retry",
      };
    case "unreachable":
      return {
        text: `${rig} is unreachable (tried 4 times). Check that it is on and connected (for a rig at a remote site, that its remote is connected).`,
        action: "retry",
      };
    case "identity":
      return {
        text: `The device at ${ctx.address || `${rig}'s address`} is not ${rig}. Commander will not touch it.`,
        action: null,
      };
    case "mtime_not_preserved":
      return {
        text:
          ctx.op === "upload"
            ? "Uploaded; timestamp not preserved, so it shows the upload time."
            : "Copied; timestamp not preserved, so it shows the copy time.",
        action: null,
      };
    case "stopped":
      return { text: stoppedText(ctx), action: "retry" };
    case "watched":
      // A reserved path met at write time: the server's sentence names the
      // path and the fix; sending it again meets the same rule.
      return { text: ctx.message || "Refused: it would stand where a watched folder must go.", action: null };
    case "not_attempted": {
      const why =
        ctx.cause === "stopped"
          ? "the operation was stopped"
          : ctx.cause === "lost"
            ? "the connection to Commander was lost"
            : ctx.cause === "identity"
              ? `Commander could not confirm it was talking to ${rig}`
              : `${rig} became unreachable`;
      return { text: `Not attempted: ${why}.`, action: "retry" };
    }
    case "error":
      return { text: ctx.message || "Unexpected error.", action: "retry" };
  }
}

/** A report heading's tail for a run the server ended early, naming the rig. */
export function abortedSuffix(aborted: FilesSummaryLine["aborted"], rig: string): string {
  switch (aborted) {
    case "unreachable":
      return ` — ${rig} became unreachable`;
    case "identity":
      return ` — Commander could not confirm it was talking to ${rig}`;
    case "":
      return "";
  }
}

function stoppedText(ctx: ReasonContext): string {
  switch (ctx.op) {
    case "copy":
      return "Stopped while copying. The destination holds either the complete file or nothing under its name; Retry copies it again.";
    case "delete":
      return "Stopped while deleting; it may already be gone. The pane shows its current state.";
    case "upload": {
      const r = ctx.rig;
      const text = `Stopped while uploading. ${r} holds either the complete file or nothing under its name; Retry writes it again from Commander; if it had finished, Retry reports that it appeared on ${r} (or changed, for a file you chose to overwrite); you can leave it.`;
      return ctx.overwriting ? `${text} The old file may already have been removed.` : text;
    }
  }
}

/**
 * The rig a cross-rig item's message names, so a row reads "‹source› is
 * unreachable" rather than always blaming the destination. The server wraps
 * each side's errors with its rig.
 */
export function rigNamedIn(
  message: string | undefined,
  candidates: string[],
  fallback: string,
): string {
  if (message) {
    for (const c of candidates) if (c && message.includes(c)) return c;
  }
  return fallback;
}

export interface OverwriteRow {
  code: FilesReasonCode;
  /** The plan conflict's ledger value, for a chose_skip row. */
  dst_ledger?: DstLedger;
}

/** Whether a report row's Overwrite asks first: when the file has no archived
 * copy, or when it changed after the check and its archive status is unknown. */
export function overwriteNeedsConfirm(row: OverwriteRow): boolean {
  if (row.code === "chose_skip") return row.dst_ledger === "unarchived";
  return row.code === "appeared" || row.code === "changed";
}

/** The inline confirmation's question. `watched` is the plan item's flag: a
 * folder Commander does not watch never gets archive wording. */
export function overwriteConfirmText(
  row: OverwriteRow & { name: string; rig: string },
  watched: boolean,
): string {
  const q = `Overwrite ${row.name} on ${row.rig}?`;
  if (row.code === "chose_skip") return `${q} It has no archived copy.`;
  return watched
    ? `${q} It changed after the check, and Commander has not checked whether it has an archived copy.`
    : `${q} It changed after the check.`;
}

/* ---- the run report ------------------------------------------------------- */

export type RowOutcome = "done" | "skipped" | "failed";

export interface ReportRow {
  rel: string;
  outcome: RowOutcome;
  /** null for a plain success. */
  code: FilesReasonCode | null;
  message?: string;
  /** The destination's state as last seen, echoed by an Overwrite. */
  dst_size?: number;
  dst_mtime?: number;
  dst_ledger?: DstLedger;
  cause?: NotAttemptedCause;
  /** A type-conflicted folder's file count. */
  files?: number;
  /** A folder the run could not create: it has no plan item to run again. */
  dir?: true;
}

/** How a run ended: with its summary, or without one after Stop or a lost
 * connection (the server writes nothing after a cancel). */
export type RunEnd = "summary" | "stopped" | "interrupted";

export interface RunReportInput {
  /** The items in the order the run processes them (the order echoed). */
  order: string[];
  lines: FilesItemLine[];
  /** The last progress line's `current`: the item that was starting. */
  lastCurrent: string | null;
  summary: FilesSummaryLine | null;
  /** The user pressed Stop (otherwise a missing summary is a lost connection). */
  stoppedByUser: boolean;
}

/** One report row from a run's item line. */
export function rowFromLine(line: FilesItemLine): ReportRow {
  const done = line.outcome === "copied" || line.outcome === "deleted";
  return {
    rel: line.rel,
    outcome: done ? "done" : line.outcome === "skipped" ? "skipped" : "failed",
    // already_gone counts as deleted and is not listed.
    code: done && line.code === "already_gone" ? null : line.code,
    message: line.message,
    dst_size: line.dst_size,
    dst_mtime: line.dst_mtime,
    cause: line.code === "not_attempted" ? "unreachable" : undefined,
  };
}

/**
 * One row per ordered item. With a summary, an item with no line succeeded. Without
 * one, the client builds the report itself: rows received keep their outcome, the
 * item that was starting is `stopped`, every later item is `not_attempted`, and
 * earlier items with no row count as done. A line for a rel outside the order is a
 * folder the run could not create; it leads the rows, as it led the stream.
 */
export function mergeRunReport(input: RunReportInput): { rows: ReportRow[]; end: RunEnd } {
  const byRel = new Map<string, FilesItemLine>();
  for (const l of input.lines) byRel.set(l.rel, l);
  const ordered = new Set(input.order);
  const folders = input.lines
    .filter((l) => !ordered.has(l.rel))
    .map((l): ReportRow => ({ ...rowFromLine(l), dir: true }));
  const aborted = input.summary?.aborted === "identity" ? "identity" : "unreachable";
  const end: RunEnd = input.summary
    ? "summary"
    : input.stoppedByUser
      ? "stopped"
      : "interrupted";
  const cause: NotAttemptedCause = end === "stopped" ? "stopped" : "lost";
  const current = input.summary || input.lastCurrent === null
    ? -1
    : input.order.indexOf(input.lastCurrent);

  const rows = input.order.map((rel, i): ReportRow => {
    const line = byRel.get(rel);
    if (line) {
      const row = rowFromLine(line);
      return row.cause ? { ...row, cause: aborted } : row;
    }
    if (end === "summary" || i < current) return { rel, outcome: "done", code: null };
    if (i === current) return { rel, outcome: "failed", code: "stopped" };
    return { rel, outcome: "failed", code: "not_attempted", cause };
  });
  return { rows: [...folders, ...rows], end };
}

/** A row worth listing in the report: anything not plainly done. */
export function isListed(row: ReportRow): boolean {
  return row.outcome !== "done" || row.code !== null;
}

/** A failed row that never ran to a result: stopped mid-item or not attempted. */
export function isNotAttempted(row: ReportRow): boolean {
  return row.outcome === "failed" && (row.code === "stopped" || row.code === "not_attempted");
}

export interface ReportCounts {
  done: number;
  skipped: number;
  /** Failed rows that ran and failed. */
  failed: number;
  /** Stopped and not-attempted rows; with `failed` they make the server's failed. */
  notAttempted: number;
}

export function reportCounts(rows: ReportRow[]): ReportCounts {
  const out: ReportCounts = { done: 0, skipped: 0, failed: 0, notAttempted: 0 };
  for (const r of rows) {
    if (isNotAttempted(r)) out.notAttempted++;
    else out[r.outcome]++;
  }
  return out;
}

const DONE_LABEL: Record<RunOp, string> = { copy: "Copied", delete: "Deleted", upload: "Uploaded" };

/** The report row's outcome pill text. */
export function rowOutcomeLabel(row: ReportRow, op: RunOp): string {
  if (row.outcome === "done") return DONE_LABEL[op];
  if (row.outcome === "skipped") return "Skipped";
  if (row.code === "stopped") return "Stopped";
  if (row.code === "not_attempted") return "Not attempted";
  return "Failed";
}

/** What a finished row action did, for the dialog's live region: one row by
 * name, several as counts. */
export function rowRunAnnouncement(rows: ReportRow[], op: RunOp): string {
  if (rows.length === 1) return `${baseName(rows[0]!.rel)}: ${rowOutcomeLabel(rows[0]!, op)}.`;
  const c = reportCounts(rows);
  const parts = [
    c.done > 0 ? `${c.done} ${DONE_LABEL[op].toLowerCase()}` : "",
    c.skipped > 0 ? `${c.skipped} skipped` : "",
    c.failed > 0 ? `${c.failed} failed` : "",
    c.notAttempted > 0 ? `${c.notAttempted} not attempted` : "",
  ].filter(Boolean);
  return `Retried ${rows.length}: ${parts.join(", ")}.`;
}

/**
 * The rows of a still-running run that already have their result, so the report
 * updates as lines arrive: every item with an item line, and every item before
 * the one now starting (an item that finishes cleanly sends no line).
 */
export function settledRows(
  order: string[],
  lines: FilesItemLine[],
  lastCurrent: string | null,
): ReportRow[] {
  const byRel = new Map(lines.map((l) => [l.rel, l]));
  const current = lastCurrent === null ? -1 : order.indexOf(lastCurrent);
  const out: ReportRow[] = [];
  order.forEach((rel, i) => {
    const line = byRel.get(rel);
    if (line) out.push(rowFromLine(line));
    else if (i < current) out.push({ rel, outcome: "done", code: null });
  });
  return out;
}

/** Replace rows by rel with a re-run's rows, keeping the report's order. */
export function applyRowUpdates(rows: ReportRow[], updates: ReportRow[]): ReportRow[] {
  const byRel = new Map(updates.map((u) => [u.rel, u]));
  return rows.map((r) => {
    const u = byRel.get(r.rel);
    if (!u) return r;
    // A chose_skip row's ledger stays known; a fresh skip carries its own state.
    return { ...u, dst_ledger: u.code === "chose_skip" ? r.dst_ledger : undefined, files: r.files };
  });
}

/* ---- run wording per operation ------------------------------------------- */

/** The progress bar's accessible name. */
export const PROGRESS_LABEL: Record<RunOp, string> = {
  copy: "Copy progress",
  delete: "Delete progress",
  upload: "Upload progress",
};

/** A report row's spinner while its row action runs. */
export const RUNNING_ROW_LABEL: Record<RunOp, string> = {
  copy: "Copying…",
  delete: "Deleting…",
  upload: "Uploading…",
};

/** The leave-page question's sentence, for whatever is running. */
export function leaveRunningText(op: RunOp | null): string {
  const what =
    op === "upload"
      ? "An upload is running"
      : op === "copy"
        ? "A copy is running on your ASIAIR"
        : op === "delete"
          ? "A delete is running on your ASIAIR"
          : "A copy or delete is running on your ASIAIR";
  return `${what}. Leaving this page stops it; the report of what was done would be lost.`;
}

/* ---- upload wording ------------------------------------------------------- */

/** "Rig › share/dir", the place an upload or a rename names. */
export function placeText(rigName: string, share: string, dir: string): string {
  return [rigName, share, ...pathSegments(dir).map((s) => s.name)].join(" › ");
}

export function sendingText(name: string): string {
  return `Sending ${name} to Commander`;
}

/** Announced once Stop has ended a send: the dialog is gone. */
export function sendStoppedText(rigName: string): string {
  return `Upload stopped. Nothing was written to ${rigName}.`;
}

/** Why Upload is disabled while the conflict's choice is Skip. */
export function uploadSkipReason(rigName: string): string {
  return `Nothing to upload: you chose Skip. Choose Overwrite to replace the file on ${rigName}.`;
}

export function sendingNote(rigName: string): string {
  return `Nothing is written to ${rigName} until you confirm the upload.`;
}

export function sendFailedText(name: string, rigName: string): string {
  return `Couldn't send ${name} to Commander: the connection was lost, or your sign-in ended. Nothing was written to ${rigName}.`;
}

/** A refusal whose body was not Commander's JSON: something in front of it
 * (nginx's 1 MB default body limit, say) answered instead. */
export function nonJsonUploadRefusal(status: number): string {
  return `Commander, or a proxy in front of it, refused the upload (HTTP ${status}).`;
}

export function uploadCheckingText(name: string, rigName: string, share: string, dir: string): string {
  return `Checking ${name} against ${placeText(rigName, share, dir)}…`;
}

export function uploadPlanTitle(name: string, size: number, rigName: string): string {
  return `Upload ${name} (${formatBytes(size)}) to ${rigName}`;
}

/** The plan's one space warning, or null when the share has room (or did not
 * say how much it has). There is no size limit and no margin rule. */
export function uploadSpaceWarning(
  size: number,
  freeBytes: number | undefined,
  share: string,
  rigName: string,
): string | null {
  if (freeBytes === undefined || size <= freeBytes) return null;
  return `This file needs ${formatBytes(size)}; ${share} on ${rigName} has ${formatBytes(freeBytes)} free.`;
}

export function uploadTypeConflictText(name: string): string {
  return `${name} can't be uploaded here: a folder has that name.`;
}

export function uploadWritingText(name: string, rigName: string): string {
  return `Writing ${name} to ${rigName}`;
}

export function uploadDoneText(name: string, size: number, rigName: string): string {
  return `Uploaded ${name} (${formatBytes(size)}) to ${rigName}`;
}

/** The result's heading for the one uploaded file. */
export function uploadReportTitle(
  end: RunEnd,
  outcome: RowOutcome | undefined,
  name: string,
  rigName: string,
): string {
  if (end === "stopped") return "Upload stopped";
  if (end === "interrupted") {
    return "Upload interrupted — lost connection to Commander, or your sign-in ended";
  }
  if (outcome === "done") return `Uploaded ${name} to ${rigName}`;
  if (outcome === "skipped") return `${name} was not uploaded to ${rigName}`;
  return `Couldn't upload ${name} to ${rigName}`;
}

/**
 * The Plan failed notice. A reserved-path refusal (`watched`) is a sentence that
 * names the path and its fix, so it leads on its own rather than trailing a
 * "Couldn't check…" it does not need.
 */
export function planFailedText(
  op: "copy" | "upload",
  code: string | undefined,
  message: string,
  fromRig: string,
  toRig: string,
): string {
  if (code === "watched") return message;
  return op === "upload"
    ? `Couldn't check the upload to ${toRig}: ${message}`
    : `Couldn't check the copy from ${fromRig} to ${toRig}: ${message}`;
}

/* ---- rename wording ------------------------------------------------------- */

export function renameDoneText(oldName: string, newName: string): string {
  return `Renamed ${oldName} to ${newName}`;
}

export const RENAME_UNCHANGED = "That is already its name.";

/**
 * The Rename dialog's inline answer to a refusal, by its code. `watched` and
 * `layout` are the server's refusal sentence, as are the codes the parent
 * already words server-side (busy, unreachable, identity, host_bound, denied).
 */
export function renameErrorText(
  code: string | undefined,
  message: string,
  ctx: { rigName: string; name: string; newName: string },
): string {
  switch (code) {
    case "exists":
      return `${ctx.rigName} already has an item named ${ctx.newName} here (names that differ only in letter case count as the same). Nothing was replaced.`;
    case "in_use":
      return `${ctx.name} is in use on ${ctx.rigName} (the ASIAIR may be writing it). Try again in a moment.`;
    case "not_found":
      return `${ctx.name} isn't in this folder any more. If the connection dropped, the rename may already have gone through; the list now shows the folder as it is.`;
    default:
      return message;
  }
}

/* ---- waiting on a device -------------------------------------------------- */

/** How long without a stream line before the dialog says it is waiting. */
export const WAITING_AFTER_MS = 5000;

/** "Waiting for A to respond…", or both rigs of a cross-rig copy. */
export function waitingText(rigs: string[]): string {
  const names = rigs.filter((r, i) => rigs.indexOf(r) === i);
  return `Waiting for ${names.join(" and ")} to respond…`;
}
