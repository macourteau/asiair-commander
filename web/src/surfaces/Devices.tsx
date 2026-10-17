/**
 * Devices — per-rig management. Lists configured rigs with pause/resume/run-now
 * commands and an expandable unclassified-frames view, a create/edit/delete rig
 * profile editor, and the discovered-but-unconfigured list with a scan and a
 * one-click "set this up" that prefills the editor from what answered on the LAN.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import {
  api,
  ApiError,
  REDACTED_SENTINEL,
  useEventSubscription,
  useSnapshot,
  type AckFrameChallenge,
  type AckGroupChallenge,
  type DiscoveredRig,
  type Frame,
  type MissingFramesPage,
  type Problem,
  type RigProfile,
  type RigReclassifiedEvent,
  type RigStatus,
  type Source,
} from "@/api";
import {
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
  Toggle,
  missingFrameView,
  rigStateView,
  showMonitoringPill,
  useToast,
  type Column,
} from "@/design";
import { FrameDetailModal } from "./FrameDetailModal";
import { CoordsCell, RotationCell } from "./AstrometryCells";
import { FrameThumb } from "./FrameThumb";
import { FrameLightbox } from "./FrameLightbox";
import { FeedStatus } from "./FeedStatus";
import { useFrameFeed, type FeedRowView } from "./useFrameFeed";
import { TransferProgress } from "./TransferProgress";
import { CleanupReadout } from "./CleanupReadout";
import { isWorking } from "../util/rigActivity";
import { MonitoringPill } from "./MonitoringPill";
import { NextFrame } from "./NextFrame";
import { RigActions } from "./RigActions";
import { WaitingToSettle } from "./WaitingToSettle";
import { rigIdFromField, splitProblems } from "../util/labels";
import { fieldErrors } from "../util/fieldErrors";
import { scanSummary } from "../util/scanSummary";
import { deviceFacts, rigNamePrefill, shortGuid, slug } from "../util/deviceLabel";
import { probeOutcome, shareOptions } from "../util/shareOptions";
import { otaRenameNote, rigFolderPreview } from "../util/rigPath";
import { reclassifyOutcome } from "../util/reclassify";
import { absoluteTime, basename, formatBytes, relativeTime } from "../util/format";
import { frameTime } from "../util/frameTime";
import { copyText } from "../util/clipboard";
import {
  addressFact,
  discoveredHeading,
  remoteNameFor,
  rigRemoteView,
  showSmbShortcut,
} from "../util/remoteRig";
import { AddRemoteDialog, RemotesSection } from "./Remotes";
import { RemotePill, RigRemoteLines } from "./RemoteRig";
import "./surfaces.css";

// The SMB shortcut only makes sense on Windows, where "\\ip" opens File Explorer.
// Computed once; guarded so a server-side render without navigator is inert.
const isWindows =
  typeof navigator !== "undefined" && /Windows/.test(navigator.userAgent);

/** isIpish reports whether s looks like an IPv4 literal — the only host worth a UNC path. */
function isIpish(s: string): boolean {
  return /^\d{1,3}(\.\d{1,3}){3}$/.test(s.trim());
}

/**
 * DeviceAddress renders the Address fact. On Windows a local device's IP is a
 * click-to-open SMB shortcut; a remote's address reads "ip via remote" as plain
 * text, since the browser's machine cannot reach that network.
 */
function DeviceAddress({
  ip,
  site,
  remoteName,
}: {
  ip: string;
  site?: string;
  remoteName?: string;
}) {
  if (!showSmbShortcut(site, isWindows)) {
    return <span className="mono">{addressFact(ip, site, remoteName)}</span>;
  }
  return <SmbAddress ip={ip} />;
}

/**
 * SmbAddress renders a device IP as a Windows click-to-open shortcut: it copies
 * the UNC path "\\ip" and best-effort tries to open File Explorer (browsers
 * usually block file:// navigation from an http page, so the copy is the reliable
 * path).
 */
function SmbAddress({ ip }: { ip: string }) {
  const toast = useToast();
  const unc = `\\\\${ip}`;
  const onClick = () => {
    void copyText(unc);
    try {
      window.open("file://" + "\\\\" + ip);
    } catch {
      // Expected: most browsers block UNC/file:// from http. The copy still works.
    }
    toast.success(
      `Copied ${unc} — if File Explorer didn't open, press Win+R, paste, Enter`,
    );
  };
  return (
    <button
      type="button"
      className="mono smb-address"
      title={`Copy ${unc} (or open it in File Explorer) to browse this device's files`}
      onClick={onClick}
    >
      {ip}
      <Icon name="external" size={14} />
    </button>
  );
}

export function Devices() {
  const toast = useToast();
  const [rigs, setRigs] = useState<RigProfile[] | null>(null);
  // The scan toast names the rigs that answered, and reads them here rather than
  // from state so it never has to be rebuilt when the list loads or changes.
  const rigsRef = useRef<RigProfile[] | null>(null);
  const [discovered, setDiscovered] = useState<DiscoveredRig[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [scanning, setScanning] = useState(false);
  const [editor, setEditor] = useState<EditorState | null>(null);
  const [addingRemote, setAddingRemote] = useState(false);
  const [focusRemote, setFocusRemote] = useState<string | null>(null);
  // A build without remote support answers 503; the remote controls then stay
  // hidden. Any other failure keeps them: the live state comes from the snapshot.
  const [remotesAvailable, setRemotesAvailable] = useState(true);

  const loadRigs = useCallback(async () => {
    try {
      const loaded = await api.listRigs();
      setRigs(loaded);
      rigsRef.current = loaded;
      setError(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Couldn't load your rigs.");
    }
  }, []);

  const loadDiscovered = useCallback(async () => {
    try {
      setDiscovered(await api.listDiscovered());
    } catch {
      setDiscovered([]); // discovery is best-effort; a failure isn't fatal here
    }
  }, []);

  useEffect(() => {
    void loadRigs();
    void loadDiscovered();
  }, [loadRigs, loadDiscovered]);

  useEffect(() => {
    api.listRemotes().catch((err: unknown) => {
      if (err instanceof ApiError && err.status === 503) setRemotesAvailable(false);
    });
  }, []);

  // A device a remote found leads from the remote's card to the same adopt flow
  // as the found-device list, with its shares probed first.
  const adoptFromRemote = useCallback(async (d: DiscoveredRig) => {
    let shares: string[] | null = null;
    try {
      shares = (await api.fetchDeviceShares(d.guid)).shares;
    } catch {
      shares = null; // best-effort, as in the found-device list
    }
    setEditor({ mode: "create", rig: fromDiscovered(d, shares), shares: shares ?? undefined });
  }, []);
  const clearFocusRemote = useCallback(() => setFocusRemote(null), []);

  const scan = useCallback(async () => {
    setScanning(true);
    try {
      const found = await api.scan();
      setDiscovered(found);
      // rigsRef, not rigs: the page scans as it loads, and this callback must not
      // be rebuilt every time the rig list changes or the mount-time scan would
      // run against a stale closure.
      toast.success(scanSummary(found, rigsRef.current));
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "The scan couldn't run.");
    } finally {
      setScanning(false);
    }
  }, [toast]);

  // Kick off one scan when the page opens so a freshly powered-on ASIAIR shows up
  // without a button press; the background loop keeps it warm thereafter.
  useEffect(() => {
    void scan();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const onSaved = useCallback(
    (verb: string) => {
      setEditor(null);
      toast.success(`Rig ${verb}.`);
      void loadRigs();
    },
    [loadRigs, toast],
  );

  // Devices are keyed by their stable guid; the registry already marks which are
  // managed, so "available" is simply the un-adopted ones.
  const available = (discovered ?? []).filter((d) => !d.managed);
  const foundHeading = discoveredHeading(available);

  return (
    <>
      <header className="surface-head">
        <div>
          <h1 className="surface-head__title">Devices</h1>
          <p className="surface-head__desc">
            Manage each ASIAIR: pause or resume its transfers, run a pull right
            now, edit how it's watched, and adopt ones found on your network.
          </p>
        </div>
        <div className="surface-actions">
          <Button
            variant="primary"
            icon={<Icon name="add" size={18} />}
            onClick={() => setEditor({ mode: "create", rig: blankForm() })}
          >
            Add a rig
          </Button>
          {remotesAvailable && (
            <Button
              icon={<Icon name="add" size={18} />}
              onClick={() => setAddingRemote(true)}
            >
              Add a remote
            </Button>
          )}
        </div>
      </header>

      {error && (
        <Card pad style={{ marginBottom: "var(--space-4)" }}>
          <p className="notice tone-problem" role="alert">
            <Icon name="problem" size={18} />
            <span>{error}</span>
          </p>
        </Card>
      )}

      <div className="section-stack">
        <section aria-labelledby="rigs-heading">
          <div className="cluster-between" style={{ marginBottom: "var(--space-3)" }}>
            <h2 className="section-title" id="rigs-heading">
              Your rigs
            </h2>
          </div>

          {!rigs ? (
            <div style={{ display: "grid", placeItems: "center", minHeight: "20vh" }}>
              <Spinner label="Loading rigs…" />
            </div>
          ) : rigs.length === 0 ? (
            <Card pad>
              <EmptyState
                icon="devices"
                title="No rigs yet"
                description="Add your ASIAIR so Commander can start pulling frames each night, or scan your network below to find one."
                action={
                  <Button
                    variant="primary"
                    icon={<Icon name="add" size={18} />}
                    onClick={() => setEditor({ mode: "create", rig: blankForm() })}
                  >
                    Add a rig
                  </Button>
                }
              />
            </Card>
          ) : (
            <ul className="rig-list list-reset">
              {rigs.map((rig) => (
                <li key={rig.id}>
                  <RigItem
                    rig={rig}
                    onEdit={() =>
                      setEditor({
                        mode: "edit",
                        rig: toForm(
                          rig,
                          (discovered ?? []).find((d) => rig.guid && d.guid === rig.guid),
                        ),
                      })
                    }
                    onDeleted={() => {
                      toast.success("Rig removed.");
                      void loadRigs();
                    }}
                  />
                </li>
              ))}
            </ul>
          )}
        </section>

        {remotesAvailable && (
          <RemotesSection
            discovered={discovered ?? []}
            focusId={focusRemote}
            onFocused={clearFocusRemote}
            onAdd={() => setAddingRemote(true)}
            onAdopt={(d) => void adoptFromRemote(d)}
          />
        )}

        <section aria-labelledby="discovered-heading">
          <div className="cluster-between" style={{ marginBottom: "var(--space-3)" }}>
            <div>
              <h2 className="section-title" id="discovered-heading">
                {foundHeading.title}
              </h2>
              <p className="section-sub">{foundHeading.sub}</p>
            </div>
            <Button
              icon={<Icon name="scan" size={18} />}
              loading={scanning}
              onClick={scan}
            >
              Scan network
            </Button>
          </div>

          {!discovered ? (
            <div style={{ display: "grid", placeItems: "center", minHeight: "12vh" }}>
              <Spinner label="Loading…" />
            </div>
          ) : available.length === 0 ? (
            <Card pad>
              <EmptyState
                icon="scan"
                title="Nothing new found"
                description="Every device that answered is already set up. Run a scan after powering on an ASIAIR to find it."
              />
            </Card>
          ) : (
            <ul className="disc-list list-reset">
              {available.map((d) => (
                <li key={d.guid}>
                  <DiscoveredItem
                    device={d}
                    onAdopt={(shares) =>
                      setEditor({
                        mode: "create",
                        rig: fromDiscovered(d, shares),
                        // undefined, not [], when the probe failed: the editor
                        // then offers to run it again instead of reporting that
                        // the device has no shares.
                        shares: shares ?? undefined,
                      })
                    }
                  />
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>

      {editor && (
        <RigEditor
          state={editor}
          existingIds={new Set((rigs ?? []).map((r) => r.id))}
          onClose={() => setEditor(null)}
          onSaved={onSaved}
        />
      )}

      {addingRemote && (
        <AddRemoteDialog
          onClose={() => setAddingRemote(false)}
          onCreated={setFocusRemote}
        />
      )}
    </>
  );
}

/* ---- Rig list item ------------------------------------------------------ */

function RigItem({
  rig,
  onEdit,
  onDeleted,
}: {
  rig: RigProfile;
  onEdit: () => void;
  onDeleted: () => void;
}) {
  const toast = useToast();
  const snapshot = useSnapshot();
  const live: RigStatus | undefined = snapshot?.rigs.find((r) => r.id === rig.id);
  const [busy, setBusy] = useState<string | null>(null);
  const [showFrames, setShowFrames] = useState(false);
  const [showMissing, setShowMissing] = useState(false);
  const [showTransfers, setShowTransfers] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [confirmReset, setConfirmReset] = useState(false);

  // A live scan_air probe couldn't confirm this device's identity — a different
  // device is answering at the rig's address, so pulls and deletes are refused.
  // The fix is to re-adopt the correct device, not a marker reset (reset only
  // handles a media-stick swap on the same device).
  const identityIssue = /identity/i.test(live?.last_error ?? "");

  const command = useCallback(
    async (verb: string, fn: () => Promise<void>, done: string) => {
      setBusy(verb);
      try {
        await fn();
        toast.success(done);
      } catch (err) {
        toast.error(
          err instanceof ApiError ? err.message : "That command didn't go through.",
        );
      } finally {
        setBusy(null);
      }
    },
    [toast],
  );

  const del = useCallback(async () => {
    setDeleting(true);
    try {
      await api.deleteRig(rig.id);
      setConfirmDelete(false);
      onDeleted();
    } catch (err) {
      toast.error(
        err instanceof ApiError ? err.message : "Couldn't remove that rig.",
      );
      setDeleting(false);
    }
  }, [rig.id, onDeleted, toast]);

  const paused = live?.paused ?? false;
  const requested = live?.requested ?? "running";
  const runUntilIdle = live?.run_until_idle ?? false;
  // Prefer the worker's resolved/last-known IP; fall back to a legacy host only
  // when it is itself an IP literal (a bare guid is never a browsable path).
  const smbIp = live?.address || (isIpish(rig.host) ? rig.host.trim() : "");
  // While its remote's tunnel is down the rig reads its remote's state, not the
  // Offline the worker wrote, which would blame the rig.
  const remotePill = live ? rigRemoteView(live).pill : null;
  const framesId = `rig-frames-${rig.id}`;
  const missingId = `rig-missing-${rig.id}`;
  const transfersId = `rig-transfers-${rig.id}`;

  return (
    <Card as="article" className="rig-item" aria-label={`Rig ${rig.name}`}>
      <div className="rig-item__head">
        <div>
          <div className="rig-item__title">
            <Icon name="rig" size={18} />
            {rig.name || rig.id}
            <RemotePill site={live?.site} remoteName={live?.remote_name} />
            {!rig.managed && (
              <StatusPill tone="neutral" icon="pause" label="Not managed" />
            )}
          </div>
          <div className="rig-item__facts">
            <span>
              Device{" "}
              <span className="mono">
                {rig.guid ? shortGuid(rig.guid) : rig.host || "binding…"}
              </span>
            </span>
            {smbIp && (
              <span>
                Address{" "}
                <DeviceAddress ip={smbIp} site={live?.site} remoteName={live?.remote_name} />
              </span>
            )}
            {rig.ota && (
              <span>
                OTA <span className="mono">{rig.ota}</span>
              </span>
            )}
            <span>
              Watching{" "}
              <span className="mono">
                {rig.sources.length > 0
                  ? rig.sources.map((s) => s.share).join(", ")
                  : "no shares yet"}
              </span>
            </span>
          </div>
        </div>
        {live && (
          <div className="rig-item__status">
            {remotePill ? (
              <StatusPill view={remotePill} />
            ) : showMonitoringPill(
              live.state,
              live.requested,
              live.run_until_idle,
              live.draining,
            ) ? (
              <MonitoringPill
                nextPollAt={live.next_poll_at}
                intervalSec={live.poll_interval_sec}
              />
            ) : (
              <StatusPill
                view={rigStateView(live.state, live.requested, live.run_until_idle)}
              />
            )}
          </div>
        )}
      </div>

      {/* Live transfer feedback, so an actively-pulling rig never reads as idle
          between files — the same current-file/progress readout as the Dashboard,
          plus a compact frames-tonight/drain line. Guarded on live and on the
          nullable current field. */}
      {live && <RigRemoteLines rig={live} />}

      {live && (
        <div className="rig-item__live">
          <TransferProgress current={live.current} />
          <CleanupReadout cleanup={live.cleanup} />
          <div className="rig-item__live-meta">
            <span>
              <span className="stat__label">Transferred tonight</span>{" "}
              <span className="stat__value">{live.frames_tonight}</span>
            </span>
            {live.drain && <span className="rig-item__drain mono">{live.drain}</span>}
          </div>
          <WaitingToSettle count={live.waiting_to_settle} />
          <NextFrame
            prediction={live.prediction}
            online={live.online}
            paused={live.paused}
            working={isWorking(live)}
          />
        </div>
      )}

      {identityIssue && (
        <div className="notice tone-problem" role="alert" style={{ marginTop: "var(--space-3)" }}>
          <Icon name="problem" size={18} />
          <span style={{ flex: 1 }}>
            Commander couldn't confirm this device's identity — a different device
            may be answering at its network address, so all pulls and deletes are
            refused. If <strong>{rig.name || rig.id}</strong> was replaced or you
            re-purposed this profile, remove it and re-adopt the correct device from
            the list below.
          </span>
          <Button
            size="sm"
            variant="secondary"
            onClick={() => setConfirmDelete(true)}
          >
            Remove rig
          </Button>
        </div>
      )}

      {/* Library reconciliation downgraded safe-to-power-off to "unknown" because
          frames are missing from the NAS while a live source could still supply
          them. Surfaced here, next to the rig's status, rather than only in the
          panel below — it warns without needing the panel expanded. */}
      {live?.missing_warning && (
        <div className="notice tone-problem" role="alert" style={{ marginTop: "var(--space-3)" }}>
          <Icon name="missing" size={18} />
          <span style={{ flex: 1 }}>{live.missing_warning}</span>
          <Button
            size="sm"
            variant="secondary"
            aria-expanded={showMissing}
            aria-controls={missingId}
            onClick={() => setShowMissing(true)}
          >
            Review missing frames
          </Button>
        </div>
      )}

      <div className="rig-item__actions">
        <RigActions
          paused={paused}
          requested={requested}
          runUntilIdle={runUntilIdle}
          busy={busy}
          onPause={() =>
            command(
              "pause",
              () => api.pauseRig(rig.id),
              "Pause requested — it takes effect after the current file.",
            )
          }
          onResume={() =>
            command(
              "resume",
              () => api.resumeRig(rig.id),
              paused ? "Rig resumed." : "Pause cancelled.",
            )
          }
          onRunNow={() =>
            command("run", () => api.runRigNow(rig.id), "Pull started.")
          }
          onStopRun={() =>
            command("stop", () => api.stopRun(rig.id), "Stop run requested.")
          }
        />
        <Button
          size="sm"
          variant="ghost"
          icon={<Icon name="edit" size={16} />}
          onClick={onEdit}
        >
          Edit
        </Button>
        <Button
          size="sm"
          variant="ghost"
          aria-expanded={showFrames}
          aria-controls={framesId}
          icon={<Icon name="reclassify" size={16} />}
          onClick={() => setShowFrames((v) => !v)}
        >
          Unclassified frames
        </Button>
        <Button
          size="sm"
          variant="ghost"
          aria-expanded={showMissing}
          aria-controls={missingId}
          icon={<Icon name="missing" size={16} />}
          onClick={() => setShowMissing((v) => !v)}
        >
          Missing frames
        </Button>
        <Button
          size="sm"
          variant="ghost"
          aria-expanded={showTransfers}
          aria-controls={transfersId}
          icon={<Icon name="nas" size={16} />}
          onClick={() => setShowTransfers((v) => !v)}
        >
          Recent transfers
        </Button>
        <Button
          size="sm"
          variant="ghost"
          icon={<Icon name="rig" size={16} />}
          onClick={() => setConfirmReset(true)}
        >
          Reset identity
        </Button>
        <div className="grow" />
        <IconButton
          label={`Remove rig ${rig.name || rig.id}`}
          icon={<Icon name="trash" />}
          onClick={() => setConfirmDelete(true)}
        />
      </div>

      {showFrames && (
        <div id={framesId} className="rig-detail">
          <UnclassifiedFrames rigId={rig.id} />
        </div>
      )}

      {showMissing && (
        <div id={missingId} className="rig-detail">
          <MissingFrames rigId={rig.id} keepOriginals={rig.keep_originals} />
        </div>
      )}

      {showTransfers && (
        <div id={transfersId} className="rig-detail">
          <RecentTransfers
            rigId={rig.id}
            rigName={rig.name || rig.id}
            keepOriginals={rig.keep_originals}
          />
        </div>
      )}

      <Dialog
        open={confirmDelete}
        onClose={() => setConfirmDelete(false)}
        title="Remove this rig?"
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirmDelete(false)}>
              Cancel
            </Button>
            <Button variant="danger" loading={deleting} onClick={del}>
              Remove rig
            </Button>
          </>
        }
      >
        <p>
          This stops Commander managing{" "}
          <strong>{rig.name || rig.id}</strong> and forgets its settings. Files
          already copied to the NAS are left untouched.
        </p>
      </Dialog>

      <Dialog
        open={confirmReset}
        onClose={() => setConfirmReset(false)}
        title="Reset this device's identity?"
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirmReset(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              loading={busy === "reset"}
              onClick={() =>
                command(
                  "reset",
                  async () => {
                    await api.resetRigIdentity(rig.id);
                    setConfirmReset(false);
                  },
                  "Identity reset — the device re-confirms on the next check.",
                )
              }
            >
              Reset identity
            </Button>
          </>
        }
      >
        <p>
          Commander identifies this ASIAIR by its device ID, confirmed with a quick
          network probe each time it connects. Reset re-confirms the device and
          clears the removable-media marker so a deliberately swapped stick
          re-adopts cleanly.
        </p>
      </Dialog>
    </Card>
  );
}

/* ---- Unclassified frames + reclassify ----------------------------------- */

function UnclassifiedFrames({ rigId }: { rigId: string }) {
  const toast = useToast();
  const [frames, setFrames] = useState<Frame[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [reclassifying, setReclassifying] = useState(false);
  // Per-row dismiss confirmation, local to this component — the rig-removal
  // confirm lives in RigItem and isn't reachable here.
  const [confirmDiscard, setConfirmDiscard] = useState<Frame | null>(null);
  const [discarding, setDiscarding] = useState(false);
  const [lightbox, setLightbox] = useState<Frame | null>(null);

  const load = useCallback(async () => {
    setFrames(null);
    try {
      const page = await api.listFrames(rigId, { state: "unclassified" });
      // A culled (missing) unclassified frame belongs to the missing panel, whose
      // Acknowledge drains its device original — not here, where Discard would null
      // its dest_path and re-strand the original permanently. Show only frames still
      // awaiting classification (no server-side param; the rows already carry the mark).
      setFrames(page.frames.filter((f) => !f.missing_seen_at));
      setError(null);
    } catch (err) {
      setError(
        err instanceof ApiError ? err.message : "Couldn't load unclassified frames.",
      );
    }
  }, [rigId]);

  useEffect(() => {
    void load();
  }, [load]);

  const discard = useCallback(async () => {
    if (!confirmDiscard) return;
    setDiscarding(true);
    try {
      await api.discardFrame(rigId, confirmDiscard.id);
      setConfirmDiscard(null);
      toast.success("Frame dismissed and cleared from the backlog.");
      await load();
    } catch (err) {
      toast.error(
        err instanceof ApiError ? err.message : "Couldn't dismiss that frame.",
      );
    } finally {
      setDiscarding(false);
    }
  }, [confirmDiscard, rigId, load, toast]);

  const reclassify = useCallback(async () => {
    setReclassifying(true);
    try {
      await api.reclassifyRig(rigId);
      toast.success("Reclassify started — this rechecks held frames against your aliases.");
      await load();
    } catch (err) {
      toast.error(
        err instanceof ApiError ? err.message : "Couldn't start reclassify.",
      );
    } finally {
      setReclassifying(false);
    }
  }, [rigId, load, toast]);

  // The request above only starts the pass — it runs on the rig's worker, and at a
  // cycle boundary if one is in flight. Report the result when it lands, and refresh
  // the table under it: a reclassify moves files already on the NAS, so this and the
  // emptying table are the only signs it ran.
  useEventSubscription("rig.reclassified", (data) => {
    const e = data as RigReclassifiedEvent | undefined;
    if (e?.rig_id !== rigId) return;
    const summary = reclassifyOutcome(e);
    if (e.moved > 0) toast.success(summary);
    else toast.info(summary);
    void load();
  });

  const columns: Column<Frame>[] = [
    {
      key: "thumb",
      header: <span className="sr-only">Preview</span>,
      cell: (f) => <FrameThumb frame={f} onOpen={setLightbox} />,
    },
    {
      key: "file",
      header: "File on the device",
      cell: (f) => <span className="mono">{f.source_path}</span>,
      width: 260,
    },
    {
      key: "type",
      header: "Header type (IMAGETYP)",
      cell: (f) => <span className="mono">{f.frame_type || "—"}</span>,
    },
    { key: "target", header: "Target", cell: (f) => f.target || "—" },
    {
      key: "seen",
      header: "Seen",
      cell: (f) => (
        <span title={absoluteTime(f.mtime)}>{relativeTime(f.mtime)}</span>
      ),
      numeric: true,
    },
    {
      key: "discard",
      header: <span className="sr-only">Dismiss</span>,
      cell: (f) => (
        <IconButton
          label={`Dismiss ${basename(f.source_path)}`}
          icon={<Icon name="trash" size={16} />}
          onClick={() => setConfirmDiscard(f)}
        />
      ),
    },
  ];

  return (
    <>
      <p className="notice tone-info">
        <Icon name="info" size={18} />
        <span className="notice__body">
          <span>
            <strong>Unclassified</strong> means a frame's{" "}
            <span className="mono">IMAGETYP</span> header didn't match any type
            Commander knows, so it's held rather than filed to the wrong place.
          </span>
          <span>
            Add the missing value to the alias table in Settings (for example,
            map <span className="mono">Light Frame</span> to{" "}
            <span className="mono">LIGHT</span>), then Reclassify to re-file the
            held frames.
          </span>
        </span>
      </p>

      <div className="cluster-between">
        <span className="text-sm text-muted">
          {frames ? `${frames.length} held on this rig` : "Loading…"}
        </span>
        <Button
          size="sm"
          icon={<Icon name="reclassify" size={16} />}
          loading={reclassifying}
          onClick={reclassify}
        >
          Reclassify held frames
        </Button>
      </div>

      {error ? (
        <p className="notice tone-problem" role="alert">
          <Icon name="problem" size={18} />
          <span>{error}</span>
        </p>
      ) : !frames ? (
        <Spinner label="Loading frames…" />
      ) : (
        <Table
          caption="Unclassified frames"
          columns={columns}
          rows={frames}
          getKey={(f) => f.id}
          empty={
            <EmptyState
              icon="safe"
              title="Nothing held"
              description="Every frame from this rig matched a known type. No aliases needed."
            />
          }
        />
      )}

      <Dialog
        open={confirmDiscard != null}
        onClose={() => setConfirmDiscard(null)}
        title="Dismiss this frame?"
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirmDiscard(null)}>
              Cancel
            </Button>
            <Button variant="danger" loading={discarding} onClick={discard}>
              Dismiss frame
            </Button>
          </>
        }
      >
        <p>
          Dismiss this permanently-stuck frame when it can't be fixed — a truncated
          or empty file that will never classify. This{" "}
          <strong>removes the copy from the NAS and can't be undone</strong>, and
          stops it counting toward the never-deletable alert.
        </p>
        <p>
          It does <strong>not</strong> delete the file from the device. If the
          device file later changes, Commander will pull it again normally.
        </p>
      </Dialog>
      <FrameLightbox frame={lightbox} onClose={() => setLightbox(null)} />
    </>
  );
}

/* ---- Missing frames (out-of-band deletion reconciliation) --------------- */

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

/** Human summary of a group challenge's `skipped` breakdown, or null when it's all zero. */
function skippedSummary(skipped: { restore_pending: number; not_verified: number; already: number }): string | null {
  const parts: string[] = [];
  if (skipped.restore_pending > 0) parts.push(`${skipped.restore_pending} with a pending restore`);
  if (skipped.not_verified > 0) parts.push(`${skipped.not_verified} not verified`);
  if (skipped.already > 0) parts.push(`${skipped.already} already acknowledged`);
  return parts.length > 0 ? parts.join(", ") : null;
}

/** Maps an acknowledge-confirm failure to its toast copy (design library-reconciliation §10.7). Pre-write
 * refusals (a bad/expired token, or a 409 reason) surface the server's own message;
 * an unclassified (likely 500) failure never claims nothing happened, since a batch
 * confirm can have partially applied. */
function ackConfirmErrorMessage(err: unknown, plural: boolean): string {
  if (err instanceof ApiError) {
    if (err.code === "ACK_CONFIRMATION_INVALID") {
      return "That confirmation expired — click Acknowledge again.";
    }
    if (err.code) return err.message;
    return plural
      ? "Something went wrong — some frames may have been acknowledged. The list has been refreshed."
      : "Something went wrong — this frame may have been acknowledged. The list has been refreshed.";
  }
  // A lost response (network drop) can't distinguish success from failure — the
  // request may have applied on the server, so don't assert it didn't. reloadOnError
  // has already refreshed the list to show the truth.
  return "The request may not have completed — the list has been refreshed.";
}

interface FrameAckState {
  frame: Frame;
  challenge: AckFrameChallenge;
}

interface GroupAckState {
  dir: string;
  challenge: AckGroupChallenge;
}

/**
 * Frames whose filed NAS copy went missing outside Commander. Grouped by
 * directory, paged, each restorable (re-pulled from the device) with a cancel for
 * a pending restore. Library linking is global and lives on the Settings page, not
 * here — the marker keys the destination root, not the rig.
 *
 * Acknowledge (per-frame and per-directory) waives the reconcile-settle window for
 * frames the operator culled on purpose (design
 * library-reconciliation §10). It is inert on a keep_originals rig, so no control
 * is rendered there.
 */
function MissingFrames({ rigId, keepOriginals }: { rigId: string; keepOriginals: boolean }) {
  const toast = useToast();
  const [page, setPage] = useState(1);
  const [data, setData] = useState<MissingFramesPage | null>(null);
  const [error, setError] = useState<string | null>(null);
  // The action in flight, keyed so only its button spins: `restore:${id}` |
  // `cancel:${id}` | `ackmint:${id}` | `ackconfirm:${id}` | `ackgroupmint:${dir}` |
  // `ackgroupconfirm:${dir}`.
  const [busy, setBusy] = useState<string | null>(null);
  const [lightbox, setLightbox] = useState<Frame | null>(null);
  const [ackFrame, setAckFrame] = useState<FrameAckState | null>(null);
  const [ackGroup, setAckGroup] = useState<GroupAckState | null>(null);

  const load = useCallback(async () => {
    setData(null);
    try {
      setData(await api.missingFrames(rigId, page));
      setError(null);
    } catch (err) {
      setError(
        err instanceof ApiError ? err.message : "Couldn't load missing frames.",
      );
    }
  }, [rigId, page]);

  useEffect(() => {
    void load();
  }, [load]);

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
        await load();
      } catch (err) {
        toast.error(
          opts?.errorMessage
            ? opts.errorMessage(err)
            : err instanceof ApiError
              ? err.message
              : "That action didn't go through.",
        );
        if (opts?.reloadOnError) await load();
      } finally {
        setBusy(null);
      }
    },
    [load, toast],
  );

  const restore = (f: Frame) =>
    act(`restore:${f.id}`, () => api.restoreFrame(rigId, f.id), "Restore requested.");
  const cancelRestore = (f: Frame) =>
    act(`cancel:${f.id}`, () => api.cancelRestore(rigId, f.id), "Restore cancelled.");

  // Mint a per-frame confirmation. `{already: true}` means the frame is already
  // acknowledged — no dialog to show, just refresh so the pill reflects it.
  const startFrameAck = useCallback(
    async (f: Frame) => {
      setBusy(`ackmint:${f.id}`);
      try {
        const res = await api.acknowledgeFrameChallenge(rigId, f.id);
        if ("already" in res) {
          await load();
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
    [rigId, load, toast],
  );

  // Mint a per-directory confirmation. A count of 0 (nothing acknowledgeable) is a
  // 409 ACK_NOTHING_TO_DO, surfaced like any other refusal below.
  const startGroupAck = useCallback(
    async (dir: string) => {
      setBusy(`ackgroupmint:${dir}`);
      try {
        const challenge = await api.acknowledgeGroupChallenge(rigId, dir);
        setAckGroup({ dir, challenge });
      } catch (err) {
        toast.error(
          err instanceof ApiError
            ? err.message
            : "Couldn't check whether this directory can be acknowledged.",
        );
        // ACK_NOTHING_TO_DO means the directory emptied out from under the chip
        // (all its rows restored/acked/re-filed) — refresh so the stale count goes.
        if (err instanceof ApiError && err.code === "ACK_NOTHING_TO_DO") await load();
      } finally {
        setBusy(null);
      }
    },
    [rigId, toast, load],
  );

  // Confirm closes its dialog on success and on an expired/invalid token (design
  // library-reconciliation §10.7: that token is dead, so leaving the dialog open
  // just invites a repeat 403) but stays open on every other refusal so the
  // operator sees why. reloadOnError is set unconditionally: a 500 from the
  // batched group confirm can have partially applied, and silently not refreshing
  // would repeat the untruthful-outcome hazard the whole confirm handshake exists
  // to avoid.
  const confirmFrameAck = useCallback(() => {
    if (!ackFrame) return;
    const { frame, challenge } = ackFrame;
    void act(
      `ackconfirm:${frame.id}`,
      async () => {
        try {
          const r = await api.acknowledgeFrameConfirm(rigId, frame.id, challenge.token);
          setAckFrame(null);
          return r;
        } catch (err) {
          if (err instanceof ApiError && err.code === "ACK_CONFIRMATION_INVALID") {
            setAckFrame(null);
          }
          throw err;
        }
      },
      (r) =>
        r?.code === "ACK_QUEUED"
          ? "The rig is busy; this may already have applied."
          : "Frame acknowledged.",
      { reloadOnError: true, errorMessage: (err) => ackConfirmErrorMessage(err, false) },
    );
  }, [ackFrame, rigId, act]);

  const confirmGroupAck = useCallback(() => {
    if (!ackGroup) return;
    const { dir, challenge } = ackGroup;
    void act(
      `ackgroupconfirm:${dir}`,
      async () => {
        try {
          const r = await api.acknowledgeGroupConfirm(rigId, dir, challenge.token);
          setAckGroup(null);
          return r;
        } catch (err) {
          if (err instanceof ApiError && err.code === "ACK_CONFIRMATION_INVALID") {
            setAckGroup(null);
          }
          throw err;
        }
      },
      (r) =>
        r?.code === "ACK_QUEUED"
          ? "The rig is busy; this may already have applied."
          : `Acknowledged ${r?.acknowledged ?? 0} frame${r?.acknowledged === 1 ? "" : "s"}.`,
      { reloadOnError: true, errorMessage: (err) => ackConfirmErrorMessage(err, true) },
    );
  }, [ackGroup, rigId, act]);

  const columns: Column<Frame>[] = [
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
    {
      key: "action",
      header: <span className="sr-only">Frame actions</span>,
      cell: (f) => (
        <div className="cluster" style={{ ["--cluster-gap" as string]: "var(--space-2)" }}>
          {f.restore_requested_at ? (
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
          )}
          {!keepOriginals &&
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
            )}
        </div>
      ),
    },
  ];

  const total = data?.total ?? 0;
  const pageSize = data?.page_size ?? 0;
  const totalPages = pageSize > 0 ? Math.max(1, Math.ceil(total / pageSize)) : 1;
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

      {data && (data.groups ?? []).length > 0 && (
        <div className="stack" style={{ ["--stack-gap" as string]: "var(--space-2)" }}>
          {(data.groups ?? []).map((g) => (
            <div key={g.dir} className="cluster-between">
              <span className="text-sm text-muted">
                <span className="mono">{g.dir}</span> · {g.count}
              </span>
              {!keepOriginals && (
                <Button
                  size="sm"
                  variant="secondary"
                  loading={busy === `ackgroupmint:${g.dir}`}
                  onClick={() => startGroupAck(g.dir)}
                >
                  Acknowledge all {g.count}
                </Button>
              )}
            </div>
          ))}
        </div>
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
            caption="Missing frames"
            columns={columns}
            rows={data.frames ?? []}
            getKey={(f) => f.id}
            empty={
              <EmptyState
                icon="safe"
                title="Nothing missing"
                description="Every frame filed for this rig is still present on the NAS."
              />
            }
          />
          {total > 0 && (
            <nav className="pager" aria-label="Missing frame pages">
              <Button
                size="sm"
                variant="secondary"
                disabled={page <= 1}
                onClick={() => setPage((p) => Math.max(1, p - 1))}
              >
                Previous
              </Button>
              <span className="pager__status" aria-live="polite">
                {total} missing · page {page} of {totalPages}
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
    </>
  );
}

/* ---- Recent transfers --------------------------------------------------- */

/**
 * The rig's most recent frames with a friendly per-frame state. Refreshes live:
 * a frame.done/frame.failed for this rig refetches immediately, and a modest poll
 * backstops silent state changes the stream doesn't carry (a grace-period delete).
 */
function RecentTransfers({
  rigId,
  rigName,
  keepOriginals,
}: {
  rigId: string;
  rigName: string;
  keepOriginals: boolean;
}) {
  // The same merge engine the fleet feed uses, scoped to this rig: it seeds from
  // this rig's newest frames across every state and overlays live pending rows,
  // the transfer bar, and completion/delete events (design features §4.6).
  const { rows, loading, error } = useFrameFeed({ rigId });
  const [selected, setSelected] = useState<Frame | null>(null);
  const [lightbox, setLightbox] = useState<Frame | null>(null);

  const columns: Column<FeedRowView>[] = [
    {
      key: "thumb",
      header: <span className="sr-only">Preview</span>,
      cell: (r) => <FrameThumb frame={r.frame} onOpen={setLightbox} />,
    },
    {
      key: "file",
      header: "Source",
      cell: (r) => <span className="mono">{basename(r.path)}</span>,
      width: 220,
    },
    {
      key: "type",
      header: "Type",
      cell: (r) => <span className="mono">{r.frame?.frame_type || "—"}</span>,
    },
    {
      key: "size",
      header: "Size",
      cell: (r) => (r.size ? formatBytes(r.size) : "—"),
      numeric: true,
    },
    {
      key: "coords",
      header: "Coordinates",
      cell: (r) => <CoordsCell astrometry={r.frame?.astrometry} />,
    },
    {
      key: "rotation",
      header: "Rotation",
      cell: (r) => <RotationCell astrometry={r.frame?.astrometry} />,
      numeric: true,
    },
    {
      key: "state",
      header: "State",
      cell: (r) => <FeedStatus row={r} onHold={keepOriginals} />,
    },
    {
      key: "when",
      header: "When",
      cell: (r) => {
        const ts = frameTime(r.frame);
        return ts ? <span title={absoluteTime(ts)}>{relativeTime(ts)}</span> : <>—</>;
      },
      numeric: true,
    },
    {
      key: "detail",
      header: <span className="sr-only">Details</span>,
      cell: (r) =>
        r.frame ? (
          <IconButton
            label={`Details for ${basename(r.path)}`}
            icon={<Icon name="info" />}
            onClick={() => setSelected(r.frame!)}
          />
        ) : null,
    },
  ];

  return error ? (
    <p className="notice tone-problem" role="alert">
      <Icon name="problem" size={18} />
      <span>{error}</span>
    </p>
  ) : loading && rows.length === 0 ? (
    <Spinner label="Loading transfers…" />
  ) : (
    <>
      <Table
        caption="Recent transfers"
        columns={columns}
        rows={rows}
        getKey={(r) => r.key}
        empty={
          <EmptyState
            icon="nas"
            title="No transfers yet"
            description="Frames copied from this rig will show up here as they're pulled."
          />
        }
      />
      <FrameDetailModal
        frame={selected}
        rigLabel={rigName}
        onHold={selected?.state === "verified" && keepOriginals}
        onClose={() => setSelected(null)}
      />
      <FrameLightbox frame={lightbox} onClose={() => setLightbox(null)} />
    </>
  );
}

/* ---- Discovered device -------------------------------------------------- */

function DiscoveredItem({
  device,
  onAdopt,
}: {
  device: DiscoveredRig;
  /** null when the probe failed: unknown, which is not the same as "none". */
  onAdopt: (shares: string[] | null) => void;
}) {
  const [shares, setShares] = useState<string[] | null>(null);
  const [loading, setLoading] = useState(false);
  const snapshot = useSnapshot();
  const remoteName = remoteNameFor(device.site, snapshot?.remotes);

  // Fetch the device's shares once so the Adopt action can prefill the editor.
  // Managed devices are already filtered out of this list, so there is nothing
  // to block here. A failure stays null rather than collapsing to an empty list:
  // the editor words the two differently and offers a retry for the first.
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    api
      .fetchDeviceShares(device.guid)
      .then((res) => {
        if (cancelled) return;
        setShares(res.shares);
      })
      .catch(() => {
        if (!cancelled) setShares(null); // best-effort; adoption can still proceed
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [device.guid]);

  const adopt = useCallback(() => {
    onAdopt(shares);
  }, [onAdopt, shares]);

  // Lead with the device's own Wi-Fi name: three kits of one generation report
  // the same model string, so the model-plus-guid label cannot tell them apart.
  // The model stays visible as a fact, but only when the ssid took the title —
  // without one the title already carries it.
  const title = rigNamePrefill(device);
  const identity = device.ssid
    ? `${device.model || "ASIAIR"} · ${shortGuid(device.guid)}`
    : shortGuid(device.guid);

  return (
    <Card as="article" className="rig-item" aria-label={`Discovered device ${title}`}>
      <div className="rig-item__head">
        <div>
          <div className="rig-item__title">
            <Icon name="nas" size={18} />
            <span
              aria-hidden
              title={device.online ? "Online" : "Offline"}
              style={{
                display: "inline-block",
                width: 8,
                height: 8,
                borderRadius: "50%",
                background: device.online ? "var(--tone-safe, #3fb950)" : "var(--tone-muted, #8b949e)",
              }}
            />
            {title}
            <RemotePill site={device.site} remoteName={remoteName} />
          </div>
          <div className="rig-item__facts">
            <span>
              Address{" "}
              <DeviceAddress ip={device.address} site={device.site} remoteName={remoteName} />
            </span>
            <span>
              Device <span className="mono">{identity}</span>
            </span>
            <span>
              Shares{" "}
              <span className="mono">
                {loading
                  ? "checking…"
                  : shares === null
                    ? "couldn't check"
                    : shares.length > 0
                      ? shares.join(", ")
                      : "none found"}
              </span>
            </span>
          </div>
        </div>
        <Button
          variant="primary"
          size="sm"
          icon={<Icon name="add" size={16} />}
          loading={loading}
          onClick={adopt}
        >
          Set up this rig
        </Button>
      </div>
    </Card>
  );
}

/* ---- Rig editor --------------------------------------------------------- */

interface SourceForm {
  share: string;
  /** One root per line. */
  roots: string;
  /** Operator opt-out: a disabled share is dropped from watch/verdict/clean/restore. */
  disabled: boolean;
  /**
   * Editor-only: the row is typing a share name rather than picking one the
   * probe found. Set when the operator asks for it, and never sent to the API.
   * A share the device does not list is still legitimate — it may be switched
   * off, or the probe may have failed — so the escape hatch is always there.
   */
  manual: boolean;
}

/** blankSource is the row "Add another share" and a fresh form start from. */
function blankSource(): SourceForm {
  return { share: "", roots: "Autorun", disabled: false, manual: false };
}

interface RigForm {
  id: string;
  name: string;
  /** Durable device identity (set by adoption, shown read-only). */
  guid: string;
  cpuId: string;
  /**
   * Editor-only identity, carried from the scan so the dialog can say which kit
   * is being configured. Not part of the profile and never sent to the API: the
   * scan is the authority on both, and they change without the rig changing.
   */
  ssid: string;
  address: string;
  model: string;
  /** Host-only address of a rig with no guid binding; shown read-only. */
  host: string;
  user: string;
  password: string;
  ota: string;
  session: string;
  sources: SourceForm[];
  managed: boolean;
  capacity_constrained: boolean;
  pull_logs: boolean;
  keep_originals: boolean;
  sync_clock: boolean;
  grace: string;
  poll_interval: string;
  retry_interval: string;
  settle_cycles: string;
  min_file_age: string;
}

interface EditorState {
  mode: "create" | "edit";
  rig: RigForm;
  /**
   * Shares the device already answered with, so adopting from a card that has
   * probed does not probe again. Absent on the manual and edit paths, where the
   * dialog offers the probe itself.
   */
  shares?: string[];
}

function blankForm(): RigForm {
  return {
    id: "",
    name: "",
    guid: "",
    cpuId: "",
    ssid: "",
    address: "",
    model: "",
    host: "",
    // ASIAIR file sharing uses well-known guest credentials, so pre-fill them.
    user: "guest",
    password: "12345678",
    ota: "",
    session: "",
    sources: [blankSource()],
    managed: true,
    capacity_constrained: false,
    pull_logs: false,
    keep_originals: false,
    sync_clock: false,
    grace: "",
    poll_interval: "",
    retry_interval: "",
    settle_cycles: "",
    min_file_age: "",
  };
}

/**
 * toForm opens a saved rig for editing. `seen` is the device's last scan reply
 * when it is in the discovery registry, which is what lets the dialog show the
 * live Wi-Fi name and address of a rig that binds by guid and therefore stores
 * neither.
 */
function toForm(rig: RigProfile, seen?: DiscoveredRig): RigForm {
  return {
    id: rig.id,
    name: rig.name,
    guid: rig.guid ?? "",
    cpuId: rig.cpuId ?? "",
    ssid: seen?.ssid ?? "",
    address: seen?.address ?? "",
    model: seen?.model ?? "",
    host: rig.host,
    user: rig.user,
    password: "",
    ota: rig.ota,
    session: rig.session,
    sources:
      rig.sources.length > 0
        ? rig.sources.map((s) => ({
            share: s.share,
            roots: (s.roots ?? []).join("\n"),
            disabled: s.disabled ?? false,
            manual: false,
          }))
        : [blankSource()],
    managed: rig.managed,
    capacity_constrained: rig.capacity_constrained,
    pull_logs: rig.pull_logs,
    keep_originals: rig.keep_originals,
    sync_clock: rig.sync_clock,
    grace: rig.grace ?? "",
    poll_interval: rig.poll_interval ?? "",
    retry_interval: rig.retry_interval ?? "",
    settle_cycles: rig.settle_cycles != null ? String(rig.settle_cycles) : "",
    min_file_age: rig.min_file_age ?? "",
  };
}

function fromDiscovered(d: DiscoveredRig, shares: string[] | null): RigForm {
  const f = blankForm();
  // The device's own Wi-Fi name, not the model string: the model is identical
  // across a generation, so prefilling it leaves an operator adding several kits
  // with several identically-named rigs.
  f.name = rigNamePrefill(d);
  f.guid = d.guid;
  f.cpuId = d.cpuId;
  f.ssid = d.ssid;
  f.address = d.address;
  f.model = d.model;
  f.host = ""; // adopted rigs bind by guid, never by address
  f.sources =
    shares && shares.length > 0
      ? shares.map((share) => ({ ...blankSource(), share }))
      : [blankSource()];
  return f;
}

function toProfile(form: RigForm, mode: "create" | "edit", id: string): RigProfile {
  const sources: Source[] = form.sources
    .filter((s) => s.share.trim())
    .map((s) => ({
      share: s.share.trim(),
      roots: s.roots
        .split(/[\n,]/)
        .map((r) => r.trim())
        .filter(Boolean),
      // Omit the field when enabled so the wire stays clean (omitempty on the backend).
      ...(s.disabled ? { disabled: true } : {}),
    }));

  const profile: RigProfile = {
    id,
    name: form.name.trim(),
    guid: form.guid.trim(),
    cpuId: form.cpuId.trim(),
    host: form.host.trim(),
    user: form.user.trim(),
    // Edit keeps the stored secret via the sentinel whenever the field is left
    // blank; create sends whatever was typed (a new rig has no prior secret to
    // preserve, so a blank password is a real, if unusual, value).
    password:
      mode === "edit" && form.password === "" ? REDACTED_SENTINEL : form.password,
    ota: form.ota.trim(),
    session: form.session.trim(),
    sources,
    managed: form.managed,
    capacity_constrained: form.capacity_constrained,
    pull_logs: form.pull_logs,
    keep_originals: form.keep_originals,
    sync_clock: form.sync_clock,
  };

  if (form.grace.trim()) profile.grace = form.grace.trim();
  if (form.poll_interval.trim()) profile.poll_interval = form.poll_interval.trim();
  if (form.retry_interval.trim()) profile.retry_interval = form.retry_interval.trim();
  if (form.min_file_age.trim()) profile.min_file_age = form.min_file_age.trim();
  if (form.settle_cycles.trim()) {
    const n = Number(form.settle_cycles);
    if (Number.isFinite(n)) profile.settle_cycles = n;
  }
  return profile;
}

function RigEditor({
  state,
  existingIds,
  onClose,
  onSaved,
}: {
  state: EditorState;
  existingIds: Set<string>;
  onClose: () => void;
  onSaved: (verb: string) => void;
}) {
  const { mode } = state;
  const [form, setForm] = useState<RigForm>(state.rig);
  // Live status of the rig being edited: a share may be disabled only while its slot
  // was structurally confirmed empty this cycle, so the toggle is offered only for a
  // share in empty_slots. A never-listed (create) or offline rig has none, so the
  // toggle stays hidden and the server guard would refuse the disable anyway.
  const snapshot = useSnapshot();
  const emptySlots = new Set(
    snapshot?.rigs.find((r) => r.id === state.rig.id)?.empty_slots ?? [],
  );
  // The light template, for the "where do this rig's frames land" line under the
  // OTA/Session pair. Only the template is wanted, and a failed load simply
  // leaves the line out: it is guidance, and configuring a rig must not depend
  // on it.
  const [lightTemplate, setLightTemplate] = useState("");
  useEffect(() => {
    let cancelled = false;
    api
      .getConfig()
      .then((cfg) => {
        if (!cancelled) setLightTemplate(cfg.templates.light);
      })
      .catch(() => {
        /* no preview rather than an error — see above */
      });
    return () => {
      cancelled = true;
    };
  }, []);
  // A rename moves nothing on disk. The path is built from OTA (and <RIG>, which
  // is the immutable id), never from the friendly name the dashboard and the live
  // feed show — so a scope swap recorded as a rename renames the rig everywhere a
  // person looks and files the new scope's whole night under the old scope's
  // folder. Say so where the rename happens, since the only other place the two
  // values sit together is a fact line on the rig row.
  const renameNote =
    mode === "edit"
      ? otaRenameNote(
          { name: state.rig.name, ota: state.rig.ota },
          { name: form.name, ota: form.ota },
        )
      : null;
  const folderPreview = rigFolderPreview(lightTemplate, {
    ota: form.ota,
    session: form.session,
    // On create the id is still blank and the server derives it from the name,
    // which is what the id field's own placeholder promises.
    rigId: form.id || slug(form.name),
  });
  const [fieldErr, setFieldErr] = useState<Record<string, string>>({});
  const [problems, setProblems] = useState<Problem[]>([]);
  const [warnings, setWarnings] = useState<Problem[]>([]);
  const [general, setGeneral] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  // What the device answered when asked to list its shares. Seeded from the
  // adopt card, which has already probed; the manual and edit paths start empty
  // and offer the probe as a button, since both know a guid to probe.
  const [probed, setProbed] = useState<string[]>(state.shares ?? []);
  const [probing, setProbing] = useState(false);
  const [probeNote, setProbeNote] = useState<string | null>(
    state.shares ? probeOutcome(state.shares) : null,
  );
  const [probeErr, setProbeErr] = useState<string | null>(null);

  const set = <K extends keyof RigForm>(key: K, value: RigForm[K]) =>
    setForm((f) => ({ ...f, [key]: value }));

  const setSource = (i: number, patch: Partial<SourceForm>) =>
    setForm((f) => ({
      ...f,
      sources: f.sources.map((s, idx) => (idx === i ? { ...s, ...patch } : s)),
    }));

  const addSource = () =>
    setForm((f) => ({ ...f, sources: [...f.sources, blankSource()] }));
  const removeSource = (i: number) =>
    setForm((f) => ({ ...f, sources: f.sources.filter((_, idx) => idx !== i) }));

  // Ask the device what shares it has. Only a guid-bound rig can be probed (the
  // endpoint resolves the address from the discovery registry), and the probe is
  // advisory throughout: a failure leaves every field typeable, because a rig
  // that is switched off or off the network must still be configurable by hand.
  const probeShares = useCallback(async () => {
    const guid = form.guid.trim();
    if (!guid) return;
    setProbing(true);
    setProbeErr(null);
    setProbeNote(null);
    try {
      const res = await api.fetchDeviceShares(guid);
      setProbed(res.shares);
      setProbeNote(probeOutcome(res.shares));
      // Fill an untouched list with everything the device has, the same as
      // adopting from a card that probed successfully. A USB-primary rig needs
      // both the Udisk and EMMC shares watched, and leaving one blank row makes
      // watching only one the path of least resistance. Anything already typed
      // is left alone — a probe must not overwrite an operator's choices.
      if (res.shares.length > 0) {
        setForm((f) =>
          f.sources.every((s) => !s.share.trim())
            ? { ...f, sources: res.shares.map((share) => ({ ...blankSource(), share })) }
            : f,
        );
      }
    } catch (err) {
      setProbeErr(
        err instanceof ApiError
          ? `${err.message} You can still type the share names by hand.`
          : "Couldn't reach the server. You can still type the share names by hand.",
      );
    } finally {
      setProbing(false);
    }
  }, [form.guid]);

  const submit = useCallback(async () => {
    setGeneral(null);
    setProblems([]);
    setWarnings([]);

    const errs: Record<string, string> = {};
    if (!form.name.trim()) errs.name = "Give this rig a name you'll recognize.";

    const id = mode === "create" ? form.id.trim() || slug(form.name) : form.id;
    if (mode === "create" && existingIds.has(id)) {
      errs.id = "A rig with this id already exists. Choose a different name or id.";
    }
    setFieldErr(errs);
    if (Object.keys(errs).length > 0) return;

    setSaving(true);
    const profile = toProfile(form, mode, id);
    try {
      if (mode === "create") await api.createRig(profile);
      else await api.updateRig(id, profile);
      onSaved(mode === "create" ? "added" : "saved");
    } catch (err) {
      if (err instanceof ApiError && err.problems) {
        const scoped = err.problems.filter((p) => {
          const owner = rigIdFromField(p.Field);
          return owner == null || owner === id;
        });
        const { errors, warnings: warns } = splitProblems(scoped);
        setProblems(errors);
        setWarnings(warns);
        setFieldErr(fieldErrors(errors, { rigId: id }));
      } else if (err instanceof ApiError) {
        setGeneral(err.message);
      } else {
        setGeneral("Couldn't reach the server. Check that Commander is running.");
      }
      setSaving(false);
    }
  }, [form, mode, existingIds, onSaved]);

  return (
    <Dialog
      open
      onClose={onClose}
      size="lg"
      title={mode === "create" ? "Add a rig" : `Edit ${state.rig.name || state.rig.id}`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" loading={saving} onClick={submit}>
            {mode === "create" ? "Add rig" : "Save changes"}
          </Button>
        </>
      }
    >
      <form
        className="stack"
        style={{ ["--stack-gap" as string]: "var(--space-4)" }}
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        {general && (
          <p className="notice tone-problem" role="alert">
            <Icon name="problem" size={18} />
            <span>{general}</span>
          </p>
        )}
        {problems.length > 0 && (
          <div className="notice tone-problem" role="alert">
            <Icon name="problem" size={18} />
            <span className="notice__body">
              <strong>These need fixing before saving:</strong>
              {problems.map((p, i) => (
                <span key={i}>{p.Message}</span>
              ))}
            </span>
          </div>
        )}
        {warnings.length > 0 && (
          <div className="notice tone-working">
            <Icon name="alert" size={18} />
            <span className="notice__body">
              <strong>Heads up:</strong>
              {warnings.map((p, i) => (
                <span key={i}>{p.Message}</span>
              ))}
            </span>
          </div>
        )}

        <div className="form-grid">
          <div className="form-grid__full">
            <Field
              label="Rig name"
              required
              help="A friendly name you'll recognize across the app."
              example="Backyard 8-inch"
              error={fieldErr.name}
            >
              {(f) => (
                <TextInput
                  {...f}
                  value={form.name}
                  onChange={(e) => set("name", e.target.value)}
                />
              )}
            </Field>
          </div>

          {form.guid ? (
            // The commit point for the name, so it has to say which kit this is:
            // the guid alone is not something anyone recognises, and someone
            // adding three ASIAIRs in a row should not have to close the dialog
            // to check. The Wi-Fi name, address and model come from the scan.
            <div className="form-grid__full">
              <Field
                label="Device"
                help="Commander tracks this ASIAIR by its stable device id, so it follows the device across reboots and DHCP changes automatically."
                example={deviceFacts({
                  ssid: form.ssid,
                  address: form.address || form.host,
                  model: form.model,
                })}
              >
                {(f) => <TextInput {...f} mono readOnly value={form.guid} />}
              </Field>
            </div>
          ) : form.host ? (
            <Field
              label="Address (legacy)"
              help="This rig still binds by address; it will switch to the device id automatically the next time it's reached."
            >
              {(f) => <TextInput {...f} mono readOnly value={form.host} />}
            </Field>
          ) : null}

          {mode === "create" && (
            <Field
              label="Rig id"
              help="A short stable key used internally. Defaults from the name."
              example="backyard-8-inch"
              error={fieldErr.id}
            >
              {(f) => (
                <TextInput
                  {...f}
                  mono
                  placeholder={slug(form.name) || "auto"}
                  value={form.id}
                  onChange={(e) => set("id", e.target.value)}
                />
              )}
            </Field>
          )}

          <Field label="Share username" help="The ASIAIR file-sharing login. Default: guest.">
            {(f) => (
              <TextInput
                {...f}
                value={form.user}
                onChange={(e) => set("user", e.target.value)}
              />
            )}
          </Field>

          <Field
            label="Share password"
            help={
              mode === "edit"
                ? "Leave blank to keep the saved password. Type a new one to change it."
                : "The ASIAIR file-sharing password. Default: 12345678."
            }
          >
            {(f) => (
              <TextInput
                {...f}
                autoComplete="off"
                placeholder={mode === "edit" ? "Unchanged" : ""}
                value={form.password}
                onChange={(e) => set("password", e.target.value)}
              />
            )}
          </Field>

          <Field
            label="OTA"
            required
            help="Your scope, used as a folder in the path. Required — the templates file by OTA, so a blank value sends every frame to unclassified."
            example="RC8"
            error={fieldErr.ota}
          >
            {(f) => (
              <TextInput
                {...f}
                value={form.ota}
                onChange={(e) => set("ota", e.target.value)}
              />
            )}
          </Field>

          <Field
            label="Session"
            required
            help="Session label used in light-frame paths (e.g. Summer2026). Required while the light template uses <SESSION>."
            example="Summer2026"
            error={fieldErr.session}
          >
            {(f) => (
              <TextInput
                {...f}
                value={form.session}
                onChange={(e) => set("session", e.target.value)}
              />
            )}
          </Field>

          {(renameNote || folderPreview) && (
            <div className="form-grid__full ota-guidance">
              {/* The note appears mid-typing, so it has to be spoken as well as
                  shown. The live region is rendered empty from the moment the
                  dialog opens — a region inserted together with its content is not
                  reliably announced — and the preview stays outside it, since
                  re-reading a path on every keystroke is chatter. */}
              <div role="status" aria-live="polite">
                {renameNote && (
                  <p className="ota-guidance__nudge">
                    <span className="ota-guidance__msg">
                      <Icon name="problem" size={16} />
                      <span>
                        Folders come from OTA, not the rig name — this rig still
                        files under <span className="mono">{form.ota.trim()}</span>.
                        Frames already on the NAS stay where they are.
                      </span>
                    </span>
                    {renameNote.suggestion && (
                      <Button size="sm" onClick={() => set("ota", renameNote.suggestion ?? "")}>
                        Use &ldquo;{renameNote.suggestion}&rdquo;
                      </Button>
                    )}
                  </p>
                )}
              </div>
              {folderPreview && (
                <p className="ota-guidance__path">
                  Lights file to <span className="mono">{folderPreview}</span>
                </p>
              )}
            </div>
          )}
        </div>

        {/* Sources */}
        <fieldset className="stack" style={{ ["--stack-gap" as string]: "var(--space-3)", border: 0, padding: 0 }}>
          <div className="cluster-between">
            <legend className="aac-field__label" style={{ padding: 0 }}>
              Shares to watch
            </legend>
            {form.guid && (
              <Button
                size="sm"
                icon={<Icon name="scan" size={16} />}
                loading={probing}
                onClick={() => void probeShares()}
              >
                {probed.length > 0 ? "Check again" : "Find shares on this device"}
              </Button>
            )}
          </div>
          <p className="aac-field__help">
            The SMB shares Commander watches for new frames, and which folders
            within each. For a USB-primary rig, watch <em>both</em> the Udisk and
            EMMC shares — a mid-session USB dropout fails over to internal storage,
            and frames written there would otherwise never be pulled.
          </p>
          {probeErr ? (
            <p className="notice tone-working">
              <Icon name="alert" size={18} />
              <span>{probeErr}</span>
            </p>
          ) : probeNote ? (
            <p className="aac-field__help">{probeNote}</p>
          ) : null}

          {form.sources.map((s, i) => (
            <div className="source-row" key={i}>
              <div className="source-row__head">
                <span className="text-sm text-muted">
                  Share {i + 1}
                  {s.disabled ? " · disabled" : ""}
                </span>
                <div style={{ display: "flex", alignItems: "center", gap: "var(--space-2)" }}>
                  {s.disabled ? (
                    // Re-enabling is always safe (it only widens what is watched), so no
                    // empty-slot check gates it.
                    <Button size="sm" onClick={() => setSource(i, { disabled: false })}>
                      Enable
                    </Button>
                  ) : emptySlots.has(s.share.trim()) ? (
                    <Button size="sm" onClick={() => setSource(i, { disabled: true })}>
                      Disable
                    </Button>
                  ) : (
                    <span
                      className="text-sm text-muted"
                      title="A slot can only be disabled while its last check found it empty (no medium inserted)."
                    >
                      Can't disable while in use
                    </span>
                  )}
                  {form.sources.length > 1 && (
                    <IconButton
                      label={`Remove share ${i + 1}`}
                      icon={<Icon name="trash" size={16} />}
                      onClick={() => removeSource(i)}
                    />
                  )}
                </div>
              </div>
              <p className="aac-field__help">
                Disable a slot you don't use — only while it's empty. Delete removes the
                share entirely; disable keeps its config.
              </p>
              <ShareNameField
                value={s.share}
                options={shareOptions(probed, [s.share])}
                manual={s.manual}
                onChange={(share) => setSource(i, { share })}
                onManual={(manual) => setSource(i, { manual })}
              />
              <Field
                label="Folders within the share"
                help="One folder per line (or comma-separated). The ASIAIR writes autorun science frames under Autorun — the default. Add Live or Plan if you capture there."
              >
                {(f) => (
                  <textarea
                    {...f}
                    className="aac-input aac-input--mono"
                    rows={2}
                    placeholder="Autorun"
                    value={s.roots}
                    onChange={(e) => setSource(i, { roots: e.target.value })}
                  />
                )}
              </Field>
            </div>
          ))}

          <div>
            <Button size="sm" icon={<Icon name="add" size={16} />} onClick={addSource}>
              Add another share
            </Button>
          </div>
        </fieldset>

        {/* Toggles */}
        <div className="stack" style={{ ["--stack-gap" as string]: "var(--space-3)" }}>
          <Toggle
            checked={form.managed}
            onChange={(v) => set("managed", v)}
            label="Managed — Commander pulls, verifies, and clears this rig"
          />
          <Toggle
            checked={form.capacity_constrained}
            onChange={(v) => set("capacity_constrained", v)}
            label="Capacity-constrained — free device space more aggressively"
          />
          <Toggle
            checked={form.pull_logs}
            onChange={(v) => set("pull_logs", v)}
            label="Pull the device's own logs alongside frames"
          />
          <div>
            <Toggle
              checked={form.keep_originals}
              onChange={(v) => set("keep_originals", v)}
              label="Keep originals (never delete from device)"
              aria-describedby="keep-originals-help"
            />
            <p id="keep-originals-help" className="aac-field__help">
              Frames are still pulled, verified, and filed to the NAS, but the
              originals are never removed from the device. Turning this off later
              deletes every original it's been holding on the next check.
            </p>
          </div>
          <div>
            <Toggle
              checked={form.sync_clock}
              onChange={(v) => set("sync_clock", v)}
              label="Sync the device clock when it drifts"
              aria-describedby="sync-clock-help"
            />
            <p id="sync-clock-help" className="aac-field__help">
              Corrects the device's clock when it is more than five minutes off. An
              ASIAIR has no battery-backed clock and keeps its firmware's build date
              until the app connects, so a rig left headless timestamps its frames
              wrongly. Commander only sets the clock while the rig is idle, and keeps
              the time zone the device reports.
            </p>
          </div>
        </div>

        {/* Cadence overrides */}
        <fieldset className="stack" style={{ ["--stack-gap" as string]: "var(--space-3)", border: 0, padding: 0 }}>
          <legend className="aac-field__label" style={{ padding: 0 }}>
            Cadence overrides (optional)
          </legend>
          <p className="aac-field__help">
            Leave blank to inherit the global defaults from Settings. Durations use
            Go-style units.
          </p>
          <div className="form-grid">
            <Field label="Grace before deleting" help="How long a copied original is kept on the device before deletion." error={fieldErr.grace}>
              {(f) => (
                <TextInput
                  {...f}
                  mono
                  invalid={!!fieldErr.grace}
                  placeholder="1h"
                  value={form.grace}
                  onChange={(e) => set("grace", e.target.value)}
                />
              )}
            </Field>
            <Field label="Poll interval" help="How often to check the device for new frames." error={fieldErr.poll_interval}>
              {(f) => (
                <TextInput
                  {...f}
                  mono
                  invalid={!!fieldErr.poll_interval}
                  placeholder="45s"
                  value={form.poll_interval}
                  onChange={(e) => set("poll_interval", e.target.value)}
                />
              )}
            </Field>
            <Field label="Retry interval" help="How long to wait before retrying after a failure." error={fieldErr.retry_interval}>
              {(f) => (
                <TextInput
                  {...f}
                  mono
                  invalid={!!fieldErr.retry_interval}
                  placeholder="5m"
                  value={form.retry_interval}
                  onChange={(e) => set("retry_interval", e.target.value)}
                />
              )}
            </Field>
            <Field label="Minimum file age" help="Wait this long after a frame stops changing before pulling it. The ASIAIR can revise a frame it has already finished writing, to add plate-solve results, so a frame being complete is not the same as it being final." error={fieldErr.min_file_age}>
              {(f) => (
                <TextInput
                  {...f}
                  mono
                  invalid={!!fieldErr.min_file_age}
                  placeholder="300s"
                  value={form.min_file_age}
                  onChange={(e) => set("min_file_age", e.target.value)}
                />
              )}
            </Field>
            <Field label="Settle cycles" help="How many quiet checks before a frame is considered complete.">
              {(f) => (
                <TextInput
                  {...f}
                  mono
                  inputMode="numeric"
                  placeholder="1"
                  value={form.settle_cycles}
                  onChange={(e) => set("settle_cycles", e.target.value)}
                />
              )}
            </Field>
          </div>
        </fieldset>
      </form>
    </Dialog>
  );
}

/**
 * ShareNameField picks one watched share. A new user has no way to know what a
 * device calls its shares, so once a probe has listed them the control is a
 * selector over what actually exists. It is never only a selector, though: the
 * probe fails on a device that is switched off or off the network, and such a
 * rig must still be configurable, so "Type a name" drops back to free text and
 * a hand-typed name is always offered back in the list.
 */
function ShareNameField({
  value,
  options,
  manual,
  onChange,
  onManual,
}: {
  value: string;
  options: string[];
  manual: boolean;
  onChange: (share: string) => void;
  onManual: (manual: boolean) => void;
}) {
  // A colon is not legal in an SMB share name, so this sentinel cannot collide
  // with one the device listed.
  const TYPE_IT = "::type::";
  if (manual || options.length === 0) {
    return (
      <Field label="Share name" help="The SMB share name as it appears on the device.">
        {(f) => (
          <>
            <TextInput
              {...f}
              mono
              placeholder="Udisk Images"
              value={value}
              onChange={(e) => onChange(e.target.value)}
            />
            {options.length > 0 && (
              <div>
                <Button variant="ghost" size="sm" onClick={() => onManual(false)}>
                  Pick from the shares found on this device
                </Button>
              </div>
            )}
          </>
        )}
      </Field>
    );
  }
  return (
    <Field
      label="Share name"
      help="The shares this device answered with. Pick one, or type a name if the share you want isn't listed."
    >
      {(f) => (
        <Select
          {...f}
          className="aac-input--mono"
          value={value}
          onChange={(e) => {
            if (e.target.value === TYPE_IT) {
              onManual(true);
              return;
            }
            onChange(e.target.value);
          }}
        >
          {!value && <option value="">Choose a share…</option>}
          {options.map((o) => (
            <option key={o} value={o}>
              {o}
            </option>
          ))}
          <option value={TYPE_IT}>Type a name…</option>
        </Select>
      )}
    </Field>
  );
}

/* ---- helpers ------------------------------------------------------------ */

