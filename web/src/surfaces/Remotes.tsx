/**
 * Remotes — the Devices page's "Your remotes" section, its Add dialog, and the
 * per-remote card (design remote-sites §10). A
 * remote is a small program on a computer at a remote site that bridges that site's
 * network to Commander; its card says how the link stands, which rigs it reaches,
 * and what it found there. Live state comes from the SSE snapshot; the API is used
 * only to pair, rename, cap, re-pair and remove. The UI calls a re-pair "Make new
 * setup files", the one name a user meets for it.
 */
import { useCallback, useEffect, useId, useRef, useState } from "react";
import {
  api,
  ApiError,
  useServerInfo,
  useSnapshot,
  type DiscoveredRig,
  type RemotePairing,
  type RemoteRegion,
  type RemoteStatus,
  type RigStatus,
} from "@/api";
import {
  Button,
  Card,
  Dialog,
  EmptyState,
  Field,
  Icon,
  Select,
  StatusPill,
  TextInput,
  useToast,
} from "@/design";
import { absoluteTime, formatUptime, relativeTime } from "../util/format";
import { base64Bytes, copyText, downloadBlob } from "../util/clipboard";
import {
  buildAgeNotice,
  devicesSeenThrough,
  latencyText,
  initialRegion,
  rateLimitText,
  regionOptions,
  relayView,
  remoteStateView,
  rigsThrough,
  stillWaitingAt,
  warningText,
} from "../util/remote";
import { rigNamePrefill, shortGuid } from "../util/deviceLabel";

/** The DOM id of a remote's card, which the Add dialog hands over to. */
export const remoteCardId = (id: string) => `remote-${id}`;

const errText = (err: unknown, fallback: string) =>
  err instanceof ApiError ? err.message : fallback;

/* ---- Section ------------------------------------------------------------- */

export function RemotesSection({
  discovered,
  focusId,
  onFocused,
  onAdd,
  onAdopt,
}: {
  discovered: DiscoveredRig[];
  /** A remote whose card should take focus once it shows (the Add hand-over). */
  focusId: string | null;
  onFocused: () => void;
  onAdd: () => void;
  onAdopt: (device: DiscoveredRig) => void;
}) {
  const snapshot = useSnapshot();
  const remotes = snapshot?.remotes ?? [];
  const rigs = snapshot?.rigs ?? [];
  const relay = relayView(snapshot?.relay);

  // The hand-over: the new remote's card appears with the next snapshot, so wait
  // for it rather than assuming it is already rendered.
  useEffect(() => {
    if (!focusId) return;
    const el = document.getElementById(remoteCardId(focusId));
    if (!el) return;
    el.scrollIntoView({ block: "start", behavior: "smooth" });
    el.focus({ preventScroll: true });
    onFocused();
  }, [focusId, remotes, onFocused]);

  return (
    <section aria-labelledby="remotes-heading">
      <div className="cluster-between" style={{ marginBottom: "var(--space-3)" }}>
        <div>
          <h2 className="section-title" id="remotes-heading">
            Your remotes
          </h2>
          <p className="section-sub">
            Small programs on a computer at a remote site, such as a Raspberry Pi, that bridge its network to
            Commander, so the ASIAIRs there work like ones at home.
          </p>
        </div>
      </div>

      {/* Commander's own relay reachability, apart from every remote, so its own
          internet being down never reads as a remote down. */}
      {relay && (
        <div className="remote-relay" role="status">
          <StatusPill view={relay} />
          {relay.help && <span className="text-sm">{relay.help}</span>}
        </div>
      )}

      {remotes.length === 0 ? (
        <Card pad>
          <EmptyState
            icon="share"
            title="No remotes yet"
            description="Add a remote to reach ASIAIRs at another site, such as a remote observatory, through a small program on a computer there, such as a Raspberry Pi."
            action={
              <Button variant="primary" icon={<Icon name="add" size={18} />} onClick={onAdd}>
                Add a remote
              </Button>
            }
          />
        </Card>
      ) : (
        <ul className="rig-list list-reset">
          {remotes.map((r) => (
            <li key={r.id}>
              <RemoteCard remote={r} rigs={rigs} discovered={discovered} onAdopt={onAdopt} />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/* ---- Card ---------------------------------------------------------------- */

function RemoteCard({
  remote: r,
  rigs,
  discovered,
  onAdopt,
}: {
  remote: RemoteStatus;
  rigs: RigStatus[];
  discovered: DiscoveredRig[];
  onAdopt: (device: DiscoveredRig) => void;
}) {
  const toast = useToast();
  const serverInfo = useServerInfo();
  const [renaming, setRenaming] = useState(false);
  const [confirmRepair, setConfirmRepair] = useState(false);
  const [repairRegion, setRepairRegion] = useState("0");
  const [repairing, setRepairing] = useState(false);
  const [pairing, setPairing] = useState<RemotePairing | null>(null);
  const [confirmRemove, setConfirmRemove] = useState(false);
  const [removing, setRemoving] = useState(false);

  const state = remoteStateView(r);
  const latency = latencyText(r);
  const notice = buildAgeNotice(r, serverInfo?.version);
  const reached = rigsThrough(r.id, rigs);
  const seen = devicesSeenThrough(r.id, discovered);
  const warnings = [...(r.warnings ?? [])].reverse(); // newest first

  // The "Still waiting?" hint is due at a fixed time; render once more then, as no
  // snapshot may arrive to do it.
  const lateAt = stillWaitingAt(r);
  const [, rerender] = useState(0);
  useEffect(() => {
    if (lateAt === null) return;
    const wait = lateAt - Date.now();
    if (wait <= 0) return;
    const t = setTimeout(() => rerender((n) => n + 1), wait);
    return () => clearTimeout(t);
  }, [lateAt]);

  const repair = useCallback(async () => {
    setRepairing(true);
    try {
      setPairing(await api.repairRemote(r.id, Number(repairRegion)));
      setConfirmRepair(false);
    } catch (err) {
      toast.error(errText(err, "Couldn't make new setup files."));
    } finally {
      setRepairing(false);
    }
  }, [r.id, repairRegion, toast]);

  const openRepair = () => {
    setRepairRegion("0"); // keep the current region unless picked again
    setConfirmRepair(true);
  };

  const remove = useCallback(async () => {
    setRemoving(true);
    try {
      await api.deleteRemote(r.id);
      setConfirmRemove(false);
      toast.success(`Remote ${r.name} removed.`);
    } catch (err) {
      toast.error(errText(err, "Couldn't remove that remote."));
    } finally {
      setRemoving(false);
    }
  }, [r.id, r.name, toast]);

  return (
    <Card
      as="article"
      className="rig-item"
      aria-label={`Remote ${r.name}`}
      id={remoteCardId(r.id)}
      tabIndex={-1}
    >
      <div className="rig-item__head">
        <div>
          <div className="rig-item__title">
            <Icon name="share" size={18} />
            {r.name}
          </div>
          <div className="rig-item__facts">
            {latency && (
              <span>
                Latency <span className="mono">{latency}</span>
              </span>
            )}
            {r.version && (
              <span>
                Version <span className="mono">{r.version}</span>
              </span>
            )}
            {r.last_hello_at ? (
              <span>
                {/* The uptime comes from a hello under this pairing, which new
                    setup files clear (version with it); the restart count is the
                    remote's own history and stays. */}
                {r.version && (
                  <>
                    Up <span className="mono">{formatUptime(r.uptime_s)}</span>
                    {" · "}
                  </>
                )}
                <span className="mono">{r.restarts}</span>{" "}
                {r.restarts === 1 ? "restart" : "restarts"}
              </span>
            ) : null}
            <span>
              Pairing <span className="mono">{r.fingerprint}</span>
            </span>
          </div>
        </div>
        <div className="rig-item__status">
          <StatusPill view={state} />
        </div>
      </div>

      {state.help && <p className="text-sm remote-help">{state.help}</p>}

      {notice && (
        <div className="notice tone-problem" role="alert" style={{ marginTop: "var(--space-3)" }}>
          <Icon name="alert" size={18} />
          <span>{notice}</span>
        </div>
      )}

      {r.state === "waiting" && (
        <div className="remote-block">
          <SetupSteps />
          <p className="text-sm">
            Lost the setup files?{" "}
            <Button
              size="sm"
              variant="secondary"
              aria-label={`Make new setup files for ${r.name}`}
              onClick={openRepair}
            >
              Make new setup files
            </Button>
          </p>
        </div>
      )}

      <div className="remote-block">
        <h3 className="remote-block__title">Rigs</h3>
        {reached.current.length === 0 && reached.last.length === 0 ? (
          <p className="text-sm">No rig has been reached through this remote yet.</p>
        ) : (
          <ul className="remote-rigs remote-rigs--named list-reset">
            {reached.current.map((rig) => (
              <li key={rig.id}>
                <Icon name="rig" size={15} /> {rig.name}{" "}
                <span className="text-sm">reached through it</span>
              </li>
            ))}
            {reached.last.map((rig) => (
              <li key={rig.id}>
                <Icon name="rig" size={15} /> {rig.name}{" "}
                <span className="text-sm">last reached through it</span>
              </li>
            ))}
          </ul>
        )}
      </div>

      {seen.length > 0 && (
        <div className="remote-block">
          <h3 className="remote-block__title">Found there, not set up</h3>
          <ul className="remote-rigs list-reset">
            {seen.map((d) => (
              <li key={d.guid}>
                <span>
                  {rigNamePrefill(d)}{" "}
                  <span className="mono text-sm">
                    {d.address} · {shortGuid(d.guid)}
                  </span>
                </span>
                <Button
                  size="sm"
                  variant="secondary"
                  icon={<Icon name="add" size={16} />}
                  aria-label={`Set up ${rigNamePrefill(d)}`}
                  onClick={() => onAdopt(d)}
                >
                  Set up this rig
                </Button>
              </li>
            ))}
          </ul>
        </div>
      )}

      <BandwidthCap remote={r} />

      {warnings.length > 0 && (
        <div className="remote-block">
          <h3 className="remote-block__title">Recent warnings</h3>
          <ul className="remote-warnings list-reset">
            {warnings.map((w, i) => (
              <li key={`${w.at}-${i}`}>
                <span title={absoluteTime(w.at)} className="mono text-sm">
                  {relativeTime(w.at)}
                </span>{" "}
                {warningText(w)}
              </li>
            ))}
          </ul>
        </div>
      )}

      <div className="rig-item__actions">
        <Button
          size="sm"
          variant="ghost"
          icon={<Icon name="edit" size={16} />}
          aria-label={`Rename remote ${r.name}`}
          onClick={() => setRenaming(true)}
        >
          Rename
        </Button>
        <Button
          size="sm"
          variant="ghost"
          aria-label={`Make new setup files for ${r.name}`}
          onClick={openRepair}
        >
          Make new setup files
        </Button>
        <div className="grow" />
        <Button
          size="sm"
          variant="ghost"
          icon={<Icon name="trash" size={16} />}
          aria-label={`Remove remote ${r.name}`}
          onClick={() => setConfirmRemove(true)}
        >
          Remove
        </Button>
      </div>

      {renaming && (
        <RenameDialog remote={r} onClose={() => setRenaming(false)} />
      )}

      <Dialog
        open={confirmRepair}
        onClose={() => setConfirmRepair(false)}
        title={`Make new setup files for ${r.name}?`}
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirmRepair(false)}>
              Cancel
            </Button>
            <Button variant="danger" loading={repairing} onClick={repair}>
              Make new setup files
            </Button>
          </>
        }
      >
        <p>
          The setup files you made before stop working right away. The remote stays
          offline until you install the new ones.
        </p>
        <RegionPicker value={repairRegion} onChange={setRepairRegion} keepRegion={r.region} />
      </Dialog>

      {pairing && (
        <Dialog
          open
          onClose={() => setPairing(null)}
          title={`New setup files for ${r.name}`}
          size="lg"
          footer={
            <Button variant="secondary" onClick={() => setPairing(null)}>
              Done
            </Button>
          }
        >
          <SetupFiles pairing={pairing} replacing />
        </Dialog>
      )}

      <Dialog
        open={confirmRemove}
        onClose={() => setConfirmRemove(false)}
        title={`Remove ${r.name}?`}
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirmRemove(false)}>
              Cancel
            </Button>
            <Button variant="danger" loading={removing} onClick={remove}>
              Remove remote
            </Button>
          </>
        }
      >
        <p>
          Commander stops reaching the rigs at this site, and they read offline
          until they are reached another way. Their settings and frames are kept.
        </p>
        <p>
          Stop the remote on its computer too: inside its{" "}
          <span className="mono">asiair-remote</span> folder, run{" "}
          <span className="mono">docker compose down</span>.
        </p>
      </Dialog>
    </Card>
  );
}

function BandwidthCap({ remote: r }: { remote: RemoteStatus }) {
  const toast = useToast();
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const start = () => {
    setValue(r.rate_limit_mbps ? String(r.rate_limit_mbps) : "");
    setError(null);
    setEditing(true);
  };

  const save = async () => {
    const mbps = value.trim() === "" ? 0 : Number(value);
    if (!Number.isFinite(mbps) || mbps < 0) {
      setError("Enter a number of MB/s, or leave it empty for no cap.");
      return;
    }
    setSaving(true);
    try {
      await api.setRemoteRateLimit(r.id, mbps);
      setEditing(false);
      toast.success(mbps > 0 ? `Capped at ${mbps} MB/s.` : "Bandwidth cap removed.");
    } catch (err) {
      setError(errText(err, "Couldn't set the bandwidth cap."));
    } finally {
      setSaving(false);
    }
  };

  if (!editing) {
    return (
      <div className="remote-block remote-cap">
        <span>
          Bandwidth cap <span className="mono">{rateLimitText(r.rate_limit_mbps)}</span>
        </span>
        <Button size="sm" variant="ghost" aria-label={`Edit the bandwidth cap of ${r.name}`} onClick={start}>
          Edit
        </Button>
      </div>
    );
  }
  return (
    <form
      className="remote-block remote-cap"
      onSubmit={(e) => {
        e.preventDefault();
        void save();
      }}
    >
      <Field
        label="Bandwidth cap (MB/s)"
        help="Limits how fast Commander pulls through this remote, to spare a shared or metered link. Empty for no cap."
        error={error}
      >
        {(f) => (
          <TextInput
            {...f}
            inputMode="decimal"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            style={{ maxWidth: 160 }}
          />
        )}
      </Field>
      <div className="cluster">
        <Button size="sm" variant="ghost" onClick={() => setEditing(false)}>
          Cancel
        </Button>
        <Button size="sm" variant="primary" type="submit" loading={saving}>
          Save
        </Button>
      </div>
    </form>
  );
}

function RenameDialog({ remote: r, onClose }: { remote: RemoteStatus; onClose: () => void }) {
  const toast = useToast();
  const [name, setName] = useState(r.name);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const save = async () => {
    if (!name.trim()) {
      setError("Give the remote a name.");
      return;
    }
    setSaving(true);
    try {
      await api.renameRemote(r.id, name.trim());
      toast.success("Remote renamed.");
      onClose();
    } catch (err) {
      setError(errText(err, "Couldn't rename that remote."));
      setSaving(false);
    }
  };

  return (
    <Dialog
      open
      onClose={onClose}
      title={`Rename ${r.name}`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" loading={saving} onClick={() => void save()}>
            Rename
          </Button>
        </>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <Field label="Name" error={error} required>
          {(f) => <TextInput {...f} value={name} onChange={(e) => setName(e.target.value)} />}
        </Field>
      </form>
    </Dialog>
  );
}

/* ---- Add dialog ---------------------------------------------------------- */

/**
 * AddRemoteDialog says in one sentence what a remote is, asks for a name (with
 * the technical requirements and the relay region behind disclosures, since the
 * setup guide covers them), and then shows the setup files exactly once. Leaving
 * hands over to the remote's card.
 */
export function AddRemoteDialog({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  /** Called with the new remote's id when the user leaves the setup files. */
  onCreated: (id: string) => void;
}) {
  const [name, setName] = useState("");
  const [nameError, setNameError] = useState<string | null>(null);
  const [region, setRegion] = useState("0");
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  const [pairing, setPairing] = useState<RemotePairing | null>(null);
  const resultHeading = useRef<HTMLHeadingElement>(null);

  // The form's Create button unmounts with the form, which would drop focus to
  // the page; move it to the result's heading so it stays in the dialog.
  useEffect(() => {
    if (pairing) resultHeading.current?.focus();
  }, [pairing]);

  const create = async () => {
    if (!name.trim()) {
      setNameError("Give the remote a name, such as the site it sits at.");
      return;
    }
    setNameError(null);
    setCreating(true);
    setCreateError(null);
    try {
      setPairing(await api.createRemote(name.trim(), Number(region)));
    } catch (err) {
      setCreateError(errText(err, "Couldn't add the remote."));
    } finally {
      setCreating(false);
    }
  };

  const done = () => {
    if (pairing) onCreated(pairing.remote.id);
    onClose();
  };

  if (pairing) {
    return (
      <Dialog
        open
        onClose={done}
        title={`Set up ${pairing.remote.name}`}
        size="lg"
        footer={
          <Button variant="secondary" onClick={done}>
            Go to the remote's card
          </Button>
        }
      >
        <h3 className="remote-block__title" ref={resultHeading} tabIndex={-1}>
          Install the setup files on the remote computer
        </h3>
        <SetupFiles pairing={pairing} />
      </Dialog>
    );
  }

  return (
    <Dialog
      open
      onClose={onClose}
      title="Add a remote"
      size="lg"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" loading={creating} onClick={() => void create()}>
            Create setup files
          </Button>
        </>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          void create();
        }}
      >
        <p>
          A remote is a small program on a computer at your dark site, such as a
          Raspberry Pi, that lets Commander reach the ASIAIRs there.
        </p>
        <p className="text-sm" style={{ marginBottom: "var(--space-4)" }}>
          Your setup guide walks you through installing it.
        </p>

        <Field
          label="Name"
          help="Shown on the remote's card and on the rigs reached through it."
          example="Hilltop observatory"
          error={nameError}
          required
        >
          {(f) => (
            <TextInput {...f} value={name} onChange={(e) => setName(e.target.value)} />
          )}
        </Field>

        <details className="remote-advanced">
          <summary>Requirements</summary>
          <ul className="remote-prereqs">
            <li>Shell access to the remote computer (SSH, remote desktop or a visit): setting up and every later fix is done there.</li>
            <li>A 64-bit Linux computer with Docker, such as a Raspberry Pi or a mini PC.</li>
          </ul>
        </details>

        <RegionPicker value={region} onChange={setRegion} />

        {createError && (
          <p className="notice tone-problem" role="alert">
            <Icon name="problem" size={18} />
            <span>{createError}</span>
          </p>
        )}
      </form>
    </Dialog>
  );
}

/**
 * RegionPicker is the Advanced relay-region choice. The list needs a netcheck,
 * which takes seconds, so it is fetched only when Advanced is opened; left closed,
 * the value stays "0", which the server reads as its default (the nearest for a
 * new remote, the current region for a re-pair).
 */
function RegionPicker({
  value,
  onChange,
  keepRegion,
}: {
  value: string;
  onChange: (v: string) => void;
  /** Re-pair: the remote's current region, offered first and kept by default. */
  keepRegion?: string;
}) {
  const [regions, setRegions] = useState<RemoteRegion[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = async () => {
    if (regions || error) return;
    try {
      const list = await api.listRemoteRegions();
      setRegions(list);
      const initial = initialRegion(list, keepRegion);
      if (initial !== "0") onChange(initial);
    } catch (err) {
      setError(errText(err, "Couldn't list the relay regions."));
    }
  };

  return (
    <details
      className="remote-advanced"
      onToggle={(e) => {
        if ((e.currentTarget as HTMLDetailsElement).open) void load();
      }}
    >
      <summary>Advanced</summary>
      <Field
        label="Relay region"
        help={
          keepRegion !== undefined
            ? "Where the remote and Commander meet when no direct path can be made. Pick another if the current one was retired or is far away."
            : "Where the remote and Commander meet when no direct path can be made. The nearest one is measured and picked for you."
        }
        error={error}
      >
        {(f) => (
          <Select
            {...f}
            value={regions ? value : "0"}
            disabled={!regions}
            onChange={(e) => onChange(e.target.value)}
            options={regionOptions(regions, keepRegion, error !== null)}
          />
        )}
      </Field>
    </details>
  );
}

/* ---- Setup files and steps ----------------------------------------------- */

/**
 * SetupFiles shows a pairing's setup files with the steps to install them. The
 * env value is a secret shown only this once: nothing selects it or copies it
 * without a click. Both files download as one zip, because a browser saves a
 * downloaded ".env" as "env" but keeps the names of a zip's entries; the raw files
 * stay behind "Show the files" for anyone who would rather paste them.
 */
function SetupFiles({ pairing, replacing = false }: { pairing: RemotePairing; replacing?: boolean }) {
  const toast = useToast();
  const envId = useId();
  const snapshot = useSnapshot();
  const live = snapshot?.remotes?.find((r) => r.id === pairing.remote.id);
  const liveState = live ? remoteStateView(live) : null;

  const copy = (what: string, s: string) => {
    void copyText(s).then(
      () => toast.success(`${what} copied.`),
      () => toast.error(`Couldn't copy the ${what.toLowerCase()}.`),
    );
  };

  return (
    <div className="remote-setup">
      <div className="notice tone-problem" role="note">
        <Icon name="alert" size={18} />
        <span>
          <strong>Secret, shown only this once.</strong> Anyone with these files can
          pose as this remote. Keep them out of chats and screenshots
          {replacing ? "; the setup files you made before no longer work" : ""}.
        </span>
      </div>

      <div>
        <Button
          variant="primary"
          icon={<Icon name="download" size={18} />}
          aria-label="Download setup files (.zip)"
          onClick={() =>
            downloadBlob(
              "asiair-remote.zip",
              new Blob([base64Bytes(pairing.setup_zip)], { type: "application/zip" }),
            )
          }
        >
          Download setup files (.zip)
        </Button>
      </div>

      <p className="text-sm">
        Pairing <span className="mono">{pairing.fingerprint}</span> — the remote logs
        the same fingerprint when it starts.
      </p>

      <SetupSteps />

      <details className="remote-advanced">
        <summary>Show the files</summary>
        <div className="remote-file">
          <div className="cluster-between">
            <span className="stat__label" id={`${envId}-label`}>
              <span className="mono">.env</span> (secret)
            </span>
            <div className="cluster">
              <Button
                size="sm"
                variant="secondary"
                aria-label="Copy the .env file"
                onClick={() => copy("The .env", pairing.env_file)}
              >
                Copy
              </Button>
            </div>
          </div>
          <pre className="remote-file__body mono" role="region" aria-labelledby={`${envId}-label`}>
            {pairing.env_file}
          </pre>
        </div>

        <div className="remote-file">
          <div className="cluster-between">
            <span className="stat__label" id={`${envId}-compose`}>
              <span className="mono">docker-compose.yml</span>
            </span>
            <div className="cluster">
              <Button
                size="sm"
                variant="secondary"
                aria-label="Copy the compose file"
                onClick={() => copy("The compose file", pairing.compose_file)}
              >
                Copy
              </Button>
            </div>
          </div>
          <pre
            className="remote-file__body remote-file__body--long mono"
            role="region"
            aria-labelledby={`${envId}-compose`}
          >
            {pairing.compose_file}
          </pre>
        </div>
      </details>

      {liveState && (
        <div className="remote-relay" role="status" aria-live="polite">
          <span className="text-sm">Now:</span>
          <StatusPill view={liveState} />
        </div>
      )}
    </div>
  );
}

/**
 * SetupSteps are the numbered steps to install a remote's setup files, in the
 * setup guide's order and words. The zip already stores .env readable by its
 * owner only, so there is no permissions step.
 */
function SetupSteps() {
  return (
    <ol className="remote-steps">
      <li>
        Unzip the setup files into the remote computer's home folder. You get a
        folder named <span className="mono">asiair-remote</span>.
      </li>
      <li>
        Inside the <span className="mono">asiair-remote</span> folder, run{" "}
        <span className="mono">docker compose up -d</span>.
      </li>
      <li>
        Wait for the remote's card to read <strong>Connected</strong>.
      </li>
    </ol>
  );
}
