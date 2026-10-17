/**
 * Settings — the global configuration surface, and the load-bearing one. Edits a
 * working copy of GET /api/config and saves it whole via PUT with optimistic
 * concurrency: the updated_at from the load is echoed back, so a stale write is
 * rejected (409) and the user is told to reload. A rejected validation (400)
 * renders its problems; errors block the save, warnings are shown alongside.
 *
 * The centerpiece is the template editor: each path template has an explicit edit
 * mode that reveals clickable (and keyboard-insertable) variable chips, paired
 * with a live preview that shows where a real recent frame would land — turning a
 * mistyped token into a visible error before a frame is ever misfiled.
 */
import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import {
  api,
  ApiError,
  CSRF_HEADER,
  readCsrfToken,
  useSnapshot,
  type Config,
  type LibraryStatus,
  type PreviewResponse,
  type Problem,
  type ProvisionStatus,
  type RigDefaults,
  type RootLinkStatus,
  type Roots,
  type Templates,
  type WebSettings,
} from "@/api";
import { useBlocker } from "react-router";
import {
  Button,
  Card,
  Dialog,
  Field,
  Icon,
  IconButton,
  Select,
  type SelectOption,
  Spinner,
  StatusPill,
  TextInput,
  Toggle,
  useToast,
} from "@/design";
import type { IconName, Tone } from "@/design";
import { AllowedHostsField } from "@/components/AllowedHostsField";
import { splitProblems } from "../util/labels";
import { fieldErrors } from "../util/fieldErrors";
import { formatBytes } from "../util/format";
import "./surfaces.css";

const GIB = 1024 ** 3;

// The validator Fields that Settings renders inline at their input. Any other
// problem (a field this page doesn't render, a per-rig field) still falls to the
// banner, so nothing is silently swallowed.
const SETTINGS_INLINE_FIELDS = new Set([
  "roots",
  "defaults.poll_interval",
  "defaults.retry_interval",
  "defaults.min_file_age",
  "defaults.grace",
  "defaults.reconcile_settle",
  "max_concurrent_transfers",
  "missing_display_period",
  "delete_sidecar_extensions",
  "plate_solve_radius_deg",
]);

export function Settings() {
  const toast = useToast();
  // Live plate-solver provisioning status rides the SSE snapshot — it is
  // read-only and separate from the editable config working copy below.
  const solverStatus = useSnapshot()?.solver_status;
  const [config, setConfig] = useState<Config | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

  // Text-edited views of array/number fields, kept local so typing is smooth.
  const [aliasRows, setAliasRows] = useState<[string, string][]>([]);
  const [sensorRows, setSensorRows] = useState<[string, string][]>([]);
  const [excludeExt, setExcludeExt] = useState("");
  const [deleteSidecarExt, setDeleteSidecarExt] = useState("");
  const [diskGiB, setDiskGiB] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");

  const [saving, setSaving] = useState(false);
  const [errors, setErrors] = useState<Problem[]>([]);
  // Per-input validation messages keyed by validator Field, so a rejected save
  // lights up the offending input inline instead of only listing it in the banner.
  const [fieldErrs, setFieldErrs] = useState<Record<string, string>>({});
  const [warnings, setWarnings] = useState<Problem[]>([]);
  // Address-list findings are pulled out of the banner lists and rendered against
  // the entry that caused them, which is where the operator has to act.
  const [hostProblems, setHostProblems] = useState<Problem[]>([]);
  const [general, setGeneral] = useState<string | null>(null);
  const [stale, setStale] = useState(false);
  // The stored value of the mosaic toggle, kept alongside the working copy. Turning
  // detection off has a consequence worth stating — panels already filed under the
  // mosaic layout stay where they are — but saying so on an install that never
  // turned it on would be noise, so the copy is keyed on the transition.
  const [savedMosaicDetection, setSavedMosaicDetection] = useState(false);

  // Baseline snapshot captured at load/save; the page is "dirty" (has unsaved
  // edits) when the current editable state no longer matches it. This is what
  // powers the unsaved-changes cue and the refresh guard, so an edit that hasn't
  // been saved — a template tweaked and "Done"'d but not saved — isn't lost
  // silently on reload.
  const baselineRef = useRef<string>("");
  const snapshotOf = (cfg: Config) =>
    JSON.stringify([
      cfg,
      Object.entries(cfg.alias_table ?? {}),
      Object.entries(cfg.sensor_map ?? {}),
      (cfg.exclude_extensions ?? []).join(", "),
      (cfg.delete_sidecar_extensions ?? []).join(", "),
      String(round2(cfg.disk_reserve_bytes / GIB)),
      "",
      "",
    ]);

  const syncDerived = useCallback((cfg: Config) => {
    setAliasRows(Object.entries(cfg.alias_table ?? {}));
    setSensorRows(Object.entries(cfg.sensor_map ?? {}));
    setExcludeExt((cfg.exclude_extensions ?? []).join(", "));
    setDeleteSidecarExt((cfg.delete_sidecar_extensions ?? []).join(", "));
    setDiskGiB(String(round2(cfg.disk_reserve_bytes / GIB)));
    setSavedMosaicDetection(cfg.mosaic_detection);
    baselineRef.current = snapshotOf(cfg);
  }, []);

  const dirty =
    config != null &&
    JSON.stringify([
      config,
      aliasRows,
      sensorRows,
      excludeExt,
      deleteSidecarExt,
      diskGiB,
      newPassword,
      confirmPassword,
    ]) !== baselineRef.current;

  // Warn before a reload/close discards unsaved edits.
  useEffect(() => {
    if (!dirty) return;
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [dirty]);

  // beforeunload only catches a reload/close; useBlocker catches in-app navigation
  // (clicking another surface) while there are unsaved edits.
  const blocker = useBlocker(dirty);

  const load = useCallback(async () => {
    setLoadError(null);
    try {
      // `problems` describes what is already stored (a hand-edited "*" entry, an
      // IP-only list) and is stripped from the config kept in state — the server
      // rejects it as an unknown field when this config is PUT back.
      const { problems, ...cfg } = await api.getConfig();
      setConfig(cfg);
      syncDerived(cfg);
      setStale(false);
      setErrors([]);
      setFieldErrs({});
      setWarnings([]);
      setHostProblems(problems ?? []);
      setGeneral(null);
      setNewPassword("");
      setConfirmPassword("");
    } catch (err) {
      setLoadError(
        err instanceof ApiError ? err.message : "Couldn't load settings.",
      );
    }
  }, [syncDerived]);

  useEffect(() => {
    void load();
  }, [load]);

  // Config setters (immutable, nested-aware).
  const patch = (p: Partial<Config>) =>
    setConfig((c) => (c ? { ...c, ...p } : c));
  const patchRoots = (p: Partial<Roots>) =>
    setConfig((c) => (c ? { ...c, roots: { ...c.roots, ...p } } : c));
  const patchDefaults = (p: Partial<RigDefaults>) =>
    setConfig((c) => (c ? { ...c, defaults: { ...c.defaults, ...p } } : c));
  const patchWeb = (p: Partial<WebSettings>) =>
    setConfig((c) => (c ? { ...c, web: { ...c.web, ...p } } : c));
  const setTemplates = (t: Templates) => patch({ templates: t });

  const save = useCallback(async () => {
    if (!config) return;
    setGeneral(null);
    setErrors([]);
    setFieldErrs({});
    setWarnings([]);
    setHostProblems([]);
    setStale(false);

    if (newPassword && newPassword !== confirmPassword) {
      setGeneral("The two new passwords don't match.");
      return;
    }
    if (newPassword && newPassword.length < 8) {
      setGeneral("A new password needs at least 8 characters.");
      return;
    }

    const outgoing: Config = {
      ...config,
      alias_table: rowsToObject(aliasRows),
      sensor_map: rowsToObject(sensorRows),
      exclude_extensions: parseCsv(excludeExt),
      delete_sidecar_extensions: parseCsv(deleteSidecarExt),
      disk_reserve_bytes: Math.max(0, Math.round(Number(diskGiB || 0) * GIB)),
      password_hash: newPassword ? newPassword : config.password_hash,
    };

    setSaving(true);
    try {
      // A save can succeed and still carry findings: a grandfathered address-list
      // entry warns rather than blocking. They render against the entry they name,
      // the same as a rejected save's errors. `problems` is stripped from the
      // config kept in state — the server rejects it as an unknown field on the
      // next PUT.
      const { problems, ...saved } = await api.putConfig(outgoing);
      setConfig(saved);
      syncDerived(saved);
      setHostProblems(problems ?? []);
      setNewPassword("");
      setConfirmPassword("");
      toast.success("Settings saved.");
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        setStale(true);
      } else if (err instanceof ApiError && err.problems) {
        const hosts = err.problems.filter((p) => p.Field === "web.allowed_hosts");
        const rest = err.problems.filter((p) => p.Field !== "web.allowed_hosts");
        const split = splitProblems(rest);
        setHostProblems(hosts);
        setErrors(split.errors);
        setFieldErrs(fieldErrors(split.errors));
        setWarnings(split.warnings);
      } else if (err instanceof ApiError) {
        setGeneral(err.message);
      } else {
        setGeneral("Couldn't reach the server. Your changes were not saved.");
      }
    } finally {
      setSaving(false);
    }
  }, [
    config,
    aliasRows,
    sensorRows,
    excludeExt,
    deleteSidecarExt,
    diskGiB,
    newPassword,
    confirmPassword,
    syncDerived,
    toast,
  ]);

  if (loadError) {
    return (
      <Card pad>
        <p className="notice tone-problem" role="alert">
          <Icon name="problem" size={18} />
          <span className="notice__body">
            <span>{loadError}</span>
            <Button size="sm" onClick={load}>
              Try again
            </Button>
          </span>
        </p>
      </Card>
    );
  }
  if (!config) {
    return (
      <div style={{ display: "grid", placeItems: "center", minHeight: "40vh" }}>
        <Spinner label="Loading settings…" />
      </div>
    );
  }

  const diskBytes = Math.max(0, Math.round(Number(diskGiB || 0) * GIB));

  // Errors shown inline at their input are dropped from the banner; only genuinely
  // unattached problems (a field this page doesn't render) remain there.
  const unattachedErrors = errors.filter((p) => !SETTINGS_INLINE_FIELDS.has(p.Field));

  return (
    <>
      <Dialog
        open={blocker.state === "blocked"}
        onClose={() => blocker.reset?.()}
        title="Discard unsaved changes?"
        footer={
          <>
            <Button variant="ghost" onClick={() => blocker.reset?.()}>
              Keep editing
            </Button>
            <Button variant="danger" onClick={() => blocker.proceed?.()}>
              Discard changes
            </Button>
          </>
        }
      >
        <p>
          You have unsaved changes to your settings. Leaving this page now will
          discard them.
        </p>
      </Dialog>

      <header className="surface-head">
        <div>
          <h1 className="surface-head__title">Settings</h1>
          <p className="surface-head__desc">
            Global rules for how frames are named, sorted, and filed. These apply
            to every rig unless a rig overrides them.
          </p>
        </div>
        <div className="surface-actions">
          {dirty && (
            <span className="unsaved-pill" role="status">
              <Icon name="edit" size={14} aria-hidden />
              Unsaved changes
            </span>
          )}
          <Button variant="primary" loading={saving} onClick={save}>
            Save settings
          </Button>
        </div>
      </header>

      {stale && (
        <div className="notice tone-working" role="alert" style={{ marginBottom: "var(--space-4)" }}>
          <Icon name="alert" size={18} />
          <span className="notice__body">
            <strong>Someone else changed the settings while you were editing.</strong>
            <span>Reload to get the latest, then reapply your change.</span>
            <span>
              <Button size="sm" onClick={load}>
                Reload settings
              </Button>
            </span>
          </span>
        </div>
      )}
      {general && (
        <div className="notice tone-problem" role="alert" style={{ marginBottom: "var(--space-4)" }}>
          <Icon name="problem" size={18} />
          <span>{general}</span>
        </div>
      )}
      {unattachedErrors.length > 0 && (
        <div className="notice tone-problem" role="alert" style={{ marginBottom: "var(--space-4)" }}>
          <Icon name="problem" size={18} />
          <span className="notice__body">
            <strong>These need fixing before settings can be saved:</strong>
            {unattachedErrors.map((p, i) => (
              <span key={i}>
                <span className="mono">{p.Field}</span> — {p.Message}
              </span>
            ))}
          </span>
        </div>
      )}
      {warnings.length > 0 && (
        <div className="notice tone-working" style={{ marginBottom: "var(--space-4)" }}>
          <Icon name="alert" size={18} />
          <span className="notice__body">
            <strong>Warnings (these don't block saving):</strong>
            {warnings.map((p, i) => (
              <span key={i}>
                <span className="mono">{p.Field}</span> — {p.Message}
              </span>
            ))}
          </span>
        </div>
      )}

      <div className="section-stack">
        {/* Destination roots */}
        <SectionCard
          title="Where files go"
          sub="The top-level folders each kind of frame is filed under."
        >
          <div className="form-grid">
            <Field label="Light frames" help="Destination root for imaging subs." example={"D:\\Astro\\Lights"} error={fieldErrs["roots"]}>
              {(f) => (
                <TextInput {...f} mono invalid={!!fieldErrs["roots"]} value={config.roots.light}
                  onChange={(e) => patchRoots({ light: e.target.value })} />
              )}
            </Field>
            <Field label="Calibration frames" help="Root for darks, flats, and bias." example={"D:\\Astro\\Calibration"} error={fieldErrs["roots"]}>
              {(f) => (
                <TextInput {...f} mono invalid={!!fieldErrs["roots"]} value={config.roots.cal}
                  onChange={(e) => patchRoots({ cal: e.target.value })} />
              )}
            </Field>
            <Field label="Staging" help="Scratch folder on the same drive where files land while copying." example={"D:\\Astro\\.staging"} error={fieldErrs["roots"]}>
              {(f) => (
                <TextInput {...f} mono invalid={!!fieldErrs["roots"]} value={config.roots.staging}
                  onChange={(e) => patchRoots({ staging: e.target.value })} />
              )}
            </Field>
            <Field label="Logs" help="Where pulled device logs are kept. May be on another drive." example={"D:\\Astro\\Logs"}>
              {(f) => (
                <TextInput {...f} mono value={config.roots.logs}
                  onChange={(e) => patchRoots({ logs: e.target.value })} />
              )}
            </Field>
          </div>
        </SectionCard>

        {/* Template editor */}
        <SectionCard
          title="Folder templates"
          sub="How each frame's folder path is built from its header. Click Edit to insert variables and preview where a real frame would land."
        >
          <TemplateEditor
            templates={config.templates}
            stripTargetSpaces={config.strip_target_spaces}
            mosaicDetection={config.mosaic_detection}
            mosaicWasEnabled={savedMosaicDetection}
            onMosaicDetectionChange={(v) => patch({ mosaic_detection: v })}
            dateFormat={config.date_format}
            pathSep={pathSeparator(config.roots)}
            onChange={setTemplates}
          />
        </SectionCard>

        {/* Alias table */}
        <SectionCard
          title="Frame-type aliases"
          sub="Maps a frame's IMAGETYP header value to a canonical type (LIGHT, DARK, FLAT, BIAS). Anything unmapped is held as unclassified."
        >
          <KVTable
            rows={aliasRows}
            onChange={setAliasRows}
            keyLabel="Header value (IMAGETYP)"
            valueLabel="Canonical type"
            keyPlaceholder="Light Frame"
            valuePlaceholder="LIGHT"
          />
        </SectionCard>

        {/* Sensor map */}
        <SectionCard
          title="Sensor names"
          sub="Maps a camera's INSTRUME header to a short name used in folder paths."
        >
          <KVTable
            rows={sensorRows}
            onChange={setSensorRows}
            keyLabel="Camera (INSTRUME)"
            valueLabel="Short name"
            keyPlaceholder="ZWO ASI2600MM Pro"
            valuePlaceholder="2600MM"
          />
        </SectionCard>

        {/* Cadence */}
        <SectionCard
          title="Timing defaults"
          sub="How often Commander checks each rig and how long it waits. A rig can override any of these."
        >
          <div className="form-grid">
            <Field label="Poll interval" help="How often to check a device for new frames." example="45s" error={fieldErrs["defaults.poll_interval"]}>
              {(f) => (
                <TextInput {...f} mono invalid={!!fieldErrs["defaults.poll_interval"]} value={config.defaults.poll_interval}
                  onChange={(e) => patchDefaults({ poll_interval: e.target.value })} />
              )}
            </Field>
            <Field label="Retry interval" help="How long to wait before retrying after a failure." example="5m" error={fieldErrs["defaults.retry_interval"]}>
              {(f) => (
                <TextInput {...f} mono invalid={!!fieldErrs["defaults.retry_interval"]} value={config.defaults.retry_interval}
                  onChange={(e) => patchDefaults({ retry_interval: e.target.value })} />
              )}
            </Field>
            <Field label="Minimum file age" help="Wait this long after a frame stops changing before pulling it. The ASIAIR can revise a frame it has already finished writing, to add plate-solve results, so a frame being complete is not the same as it being final." example="300s" error={fieldErrs["defaults.min_file_age"]}>
              {(f) => (
                <TextInput {...f} mono invalid={!!fieldErrs["defaults.min_file_age"]} value={config.defaults.min_file_age}
                  onChange={(e) => patchDefaults({ min_file_age: e.target.value })} />
              )}
            </Field>
            <Field label="Grace before deleting" help="How long a copied original is kept on the device before deletion." example="1h" error={fieldErrs["defaults.grace"]}>
              {(f) => (
                <TextInput {...f} mono invalid={!!fieldErrs["defaults.grace"]} value={config.defaults.grace}
                  onChange={(e) => patchDefaults({ grace: e.target.value })} />
              )}
            </Field>
            <Field label="Reconcile settle" help="How long a frame must stay missing from the NAS before Commander acts on it, so a mid-copy or briefly-offline share isn't mistaken for a deletion. Lower it to cull the device sooner; there is no floor, so keep it comfortably longer than a scan interval." example="24h" error={fieldErrs["defaults.reconcile_settle"]}>
              {(f) => (
                <TextInput {...f} mono invalid={!!fieldErrs["defaults.reconcile_settle"]} value={config.defaults.reconcile_settle}
                  onChange={(e) => patchDefaults({ reconcile_settle: e.target.value })} />
              )}
            </Field>
            <Field label="Settle cycles" help="How many quiet checks before a frame is considered complete." example="1">
              {(f) => (
                <TextInput {...f} mono inputMode="numeric" value={String(config.defaults.settle_cycles)}
                  onChange={(e) => patchDefaults({ settle_cycles: toInt(e.target.value, config.defaults.settle_cycles) })} />
              )}
            </Field>
          </div>
        </SectionCard>

        {/* Files & transfers */}
        <SectionCard
          title="Files and transfers"
          sub="What gets copied, what space is kept free, and how many transfers run at once."
        >
          <div className="stack" style={{ ["--stack-gap" as string]: "var(--space-4)" }}>
            <Field
              label="Strip spaces from target names"
              help="Remove spaces from a target's folder name so paths stay tidy."
              example="'NGC 7000' becomes 'NGC7000'"
            >
              {() => (
                <Toggle
                  checked={config.strip_target_spaces}
                  onChange={(v) => patch({ strip_target_spaces: v })}
                  label={config.strip_target_spaces ? "On" : "Off"}
                />
              )}
            </Field>

            <div className="form-grid">
              <Field
                label="Maximum transfers at once"
                help="Caps how many frames copy in parallel across all rigs. Takes effect after a restart."
                example="3"
                error={fieldErrs["max_concurrent_transfers"]}
              >
                {(f) => (
                  <TextInput {...f} mono inputMode="numeric" invalid={!!fieldErrs["max_concurrent_transfers"]} value={String(config.max_concurrent_transfers)}
                    onChange={(e) => patch({ max_concurrent_transfers: toInt(e.target.value, config.max_concurrent_transfers) })} />
                )}
              </Field>
              <Field
                label="Keep this much free on the NAS (GiB)"
                help="Commander stops filling the destination once free space drops to this."
                example={`${diskGiB || "0"} GiB = ${formatBytes(diskBytes)}`}
              >
                {(f) => (
                  <TextInput {...f} mono inputMode="decimal" value={diskGiB}
                    onChange={(e) => setDiskGiB(e.target.value)} />
                )}
              </Field>
              <Field
                label="Show missing frames for"
                help="How long a frame found missing from the NAS stays in the rig's missing view before it ages out of the list. 0 shows all missing frames indefinitely. This only trims the list; an aged-out frame still counts toward safe-to-power-off."
                example="0"
                error={fieldErrs["missing_display_period"]}
              >
                {(f) => (
                  <TextInput {...f} mono invalid={!!fieldErrs["missing_display_period"]} value={config.missing_display_period}
                    onChange={(e) => patch({ missing_display_period: e.target.value })} />
                )}
              </Field>
              <Field
                label="Ignore these extensions"
                help="Files with these extensions are never transferred (previews and thumbnails)."
                example="jpg, jpeg"
              >
                {(f) => (
                  <TextInput {...f} mono value={excludeExt}
                    onChange={(e) => setExcludeExt(e.target.value)} />
                )}
              </Field>
              <Field
                label="Delete these sidecars with the frame"
                help="Sidecar extensions removed alongside a frame. Each must also be in the ignore list, and never a science extension."
                example="jpg, jpeg"
                error={fieldErrs["delete_sidecar_extensions"]}
              >
                {(f) => (
                  <TextInput {...f} mono invalid={!!fieldErrs["delete_sidecar_extensions"]} value={deleteSidecarExt}
                    onChange={(e) => setDeleteSidecarExt(e.target.value)} />
                )}
              </Field>
            </div>
          </div>
        </SectionCard>

        {/* Library links */}
        <SectionCard
          title="Library"
          sub="Each destination root links itself the first time a frame is filed and verified there, which is how Commander confirms your NAS is really mounted before it ever reconciles or deletes files. Re-link a root here if its marker went missing."
        >
          <LibrarySection />
        </SectionCard>

        {/* Plate solving */}
        <SectionCard
          title="Plate solving"
          sub="Solve each light frame's true pointing and orientation on the server, and manage the star catalogs the solver reads."
        >
          <div className="stack" style={{ ["--stack-gap" as string]: "var(--space-4)" }}>
            <Field
              label="Solve light frames"
              help="Compute each light frame's WCS from its stars. Takes effect after a restart."
              example={config.plate_solve_enabled ? "On" : "Off"}
            >
              {() => (
                <Toggle
                  checked={config.plate_solve_enabled}
                  onChange={(v) => patch({ plate_solve_enabled: v })}
                  label={config.plate_solve_enabled ? "On" : "Off"}
                />
              )}
            </Field>

            <div className="form-grid">
              <Field
                label="Search radius (degrees)"
                help="How far around the pointing hint to search (degrees). 1–60; 10 is a good default."
                example="10"
                error={fieldErrs["plate_solve_radius_deg"]}
              >
                {(f) => (
                  <TextInput {...f} mono inputMode="numeric" invalid={!!fieldErrs["plate_solve_radius_deg"]} value={String(config.plate_solve_radius_deg)}
                    onChange={(e) => patch({ plate_solve_radius_deg: toInt(e.target.value, config.plate_solve_radius_deg) })} />
                )}
              </Field>
            </div>

            <CatalogSection solver={solverStatus} />
          </div>
        </SectionCard>

        {/* Web password */}
        <SectionCard
          title="Web password"
          sub="The shared password used to sign in to Commander. Leave blank to keep the current one."
        >
          <div className="form-grid">
            <Field label="New password" help="At least 8 characters.">
              {(f) => (
                <TextInput {...f} type="password" autoComplete="new-password"
                  placeholder="Unchanged" value={newPassword}
                  onChange={(e) => setNewPassword(e.target.value)} />
              )}
            </Field>
            <Field label="Confirm new password">
              {(f) => (
                <TextInput {...f} type="password" autoComplete="new-password"
                  placeholder="Unchanged" value={confirmPassword}
                  onChange={(e) => setConfirmPassword(e.target.value)} />
              )}
            </Field>
          </div>
        </SectionCard>

        {/* Allowed addresses */}
        <SectionCard
          title="Addresses you use to reach Commander"
          sub="Commander only answers to addresses listed here, which is what stops a malicious web page from talking to it behind your back. Add any name or address you'll open Commander with — its hostname, its IP, or a domain you put in front of it."
        >
          <AllowedHostsField
            value={config.web.allowed_hosts ?? []}
            onChange={(hosts) => patchWeb({ allowed_hosts: hosts })}
            label="Addresses"
            help="Keep the address you're using right now in the list: removing it would lock you out of these settings."
            problems={hostProblems}
          />
        </SectionCard>

        {/* Diagnostics */}
        <SectionCard
          title="Diagnostics"
          sub="Capture a CPU profile to diagnose a slow or busy server. The profile downloads as a .pprof file you can open with Go's pprof tooling."
        >
          <DiagnosticsSection />
        </SectionCard>

        <div className="cluster-between">
          <span className="text-sm text-muted">
            Changes apply to every rig. The transfer cap needs a restart to take effect.
          </span>
          <div className="cluster" style={{ ["--cluster-gap" as string]: "var(--space-3)" }}>
            {dirty && (
              <span className="unsaved-pill" role="status">
                <Icon name="edit" size={14} aria-hidden />
                Unsaved changes
              </span>
            )}
            <Button variant="primary" loading={saving} onClick={save}>
              Save settings
            </Button>
          </div>
        </div>
      </div>
    </>
  );
}

/* ---- Section wrapper ---------------------------------------------------- */

function SectionCard({
  title,
  sub,
  children,
}: {
  title: string;
  sub: string;
  children: React.ReactNode;
}) {
  return (
    <Card as="section" pad>
      <header style={{ marginBottom: "var(--space-4)" }}>
        <h2 className="section-title">{title}</h2>
        <p className="section-sub">{sub}</p>
      </header>
      {children}
    </Card>
  );
}

/* ---- Library links (out-of-band deletion reconciliation) ---------------- */

/** The hidden marker Commander writes into a linked destination root, shown to the
 * operator verbatim in the re-link dialog so they recognize the file they removed. */
const LIBRARY_MARKER_NAME = ".asiair-commander-library-id";

/**
 * Presentation for one root's link state: color+icon+text plus whether a Re-link
 * action applies. A never-linked root links itself on the next filed frame, so it
 * offers no button — only the informational note. A recorded-but-unmarked root (a
 * deleted sentinel or an unmounted NAS) and any corrupt/foreign marker do.
 */
function rootLinkView(s: RootLinkStatus): {
  tone: Tone;
  icon: IconName;
  label: string;
  canRelink: boolean;
  note?: string;
} {
  if (s.linked) {
    return { tone: "safe", icon: "safe", label: "Linked", canRelink: false };
  }
  switch (s.reason) {
    case "not linked":
      return {
        tone: "info",
        icon: "info",
        label: "Links automatically",
        canRelink: false,
        note: "This root links itself the first time a frame is filed and verified here. Nothing to do.",
      };
    case "marker missing":
      return {
        tone: "problem",
        icon: "missing",
        label: "Marker missing — re-link",
        canRelink: true,
        note: "The marker file is gone. If your NAS is unmounted it returns on its own when the NAS remounts; if the file was deleted, re-link to restore it.",
      };
    case "different library":
      return {
        tone: "problem",
        icon: "problem",
        label: "Different library",
        canRelink: true,
        note: "Another installation's marker is present at this root. Re-link only if this destination really is this Commander's.",
      };
    default:
      // "marker corrupt", "marker unreadable", or a reason a newer backend adds.
      return {
        tone: "problem",
        icon: "problem",
        label: "Marker unreadable — re-link",
        canRelink: true,
        note: "The marker couldn't be read. Re-link to rewrite it.",
      };
  }
}

/**
 * The global library section: every configured destination root's link state, and a
 * re-link path for a root whose marker went missing (a deleted sentinel). Linking is
 * automatic on the first filed frame, so the common case shows only "Linked" — this
 * exists for recovery. The link/rescan endpoints are per-rig but the marker keys the
 * root, so any managed rig id performs the operation for the whole fleet.
 */
function LibrarySection() {
  const toast = useToast();
  const [status, setStatus] = useState<LibraryStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [rigId, setRigId] = useState<string | null>(null);
  // The root whose Re-link confirmation dialog is open, if any.
  const [confirmRoot, setConfirmRoot] = useState<string | null>(null);
  const [busy, setBusy] = useState<"relink" | "rescan" | null>(null);

  const load = useCallback(async () => {
    try {
      setStatus(await api.libraryStatus());
      setError(null);
    } catch (err) {
      setError(
        err instanceof ApiError ? err.message : "Couldn't load library status.",
      );
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // Resolve a managed rig once to route the (global) link/rescan calls through.
  useEffect(() => {
    let cancelled = false;
    api
      .listRigs()
      .then((rigs) => {
        if (!cancelled) setRigId(rigs.find((r) => r.managed)?.id ?? null);
      })
      .catch(() => {
        if (!cancelled) setRigId(null);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const relink = useCallback(
    async (root: string) => {
      if (!rigId) return;
      setBusy("relink");
      try {
        await api.linkLibrary(rigId, root);
        setConfirmRoot(null);
        toast.success("Library re-linked — the marker was rewritten.");
        await load();
      } catch (err) {
        toast.error(
          err instanceof ApiError ? err.message : "Couldn't re-link that root.",
        );
      } finally {
        setBusy(null);
      }
    },
    [rigId, load, toast],
  );

  const rescan = useCallback(async () => {
    if (!rigId) return;
    setBusy("rescan");
    try {
      await api.rescanLibrary(rigId);
      toast.success("Rescan started — the library state updates as it runs.");
    } catch (err) {
      toast.error(
        err instanceof ApiError ? err.message : "Couldn't start a rescan.",
      );
    } finally {
      setBusy(null);
    }
  }, [rigId, toast]);

  if (error) {
    return (
      <p className="notice tone-problem" role="alert">
        <Icon name="problem" size={18} />
        <span className="notice__body">
          <span>{error}</span>
          <Button size="sm" onClick={load}>
            Try again
          </Button>
        </span>
      </p>
    );
  }
  if (!status) {
    return <Spinner label="Loading library status…" />;
  }

  // A re-link or rescan must run through a managed rig; without one, disable the
  // actions and say why rather than failing on click.
  const noRig = rigId == null;

  return (
    <div className="stack" style={{ ["--stack-gap" as string]: "var(--space-4)" }}>
      {status.roots.length === 0 ? (
        <p className="text-sm text-muted">
          No destination roots are configured yet. Set your light and calibration
          roots above; each links itself once frames are filed there.
        </p>
      ) : (
        <div className="stack" style={{ ["--stack-gap" as string]: "var(--space-3)" }}>
          {status.roots.map((r) => {
            const view = rootLinkView(r);
            return (
              <div className="cluster-between" key={r.root}>
                <div className="stack" style={{ ["--stack-gap" as string]: "var(--space-1)" }}>
                  <span className="mono">{r.root}</span>
                  {view.note && <span className="text-sm text-muted">{view.note}</span>}
                </div>
                <div className="cluster" style={{ ["--cluster-gap" as string]: "var(--space-3)" }}>
                  <StatusPill tone={view.tone} icon={view.icon} label={view.label} />
                  {view.canRelink && (
                    <Button
                      size="sm"
                      variant="secondary"
                      disabled={noRig}
                      title={
                        noRig
                          ? "Add a managed rig first — re-linking runs through one."
                          : undefined
                      }
                      onClick={() => setConfirmRoot(r.root)}
                    >
                      Re-link
                    </Button>
                  )}
                </div>
              </div>
            );
          })}
        </div>
      )}

      <div className="cluster" style={{ ["--cluster-gap" as string]: "var(--space-3)" }}>
        <Button
          size="sm"
          variant="secondary"
          icon={<Icon name="run-now" size={16} />}
          loading={busy === "rescan"}
          disabled={noRig}
          title={
            noRig ? "Add a managed rig first — a rescan runs through one." : undefined
          }
          onClick={rescan}
        >
          Rescan now
        </Button>
        <span className="text-sm text-muted">
          Re-check every linked root against the NAS now instead of waiting for the
          next scheduled scan.
        </span>
      </div>

      <Dialog
        open={confirmRoot != null}
        onClose={() => setConfirmRoot(null)}
        title="Re-link this library root?"
        footer={
          <>
            <Button variant="ghost" onClick={() => setConfirmRoot(null)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              loading={busy === "relink"}
              onClick={() => confirmRoot && void relink(confirmRoot)}
            >
              Re-link
            </Button>
          </>
        }
      >
        <p>
          This recreates a hidden file named{" "}
          <span className="mono">{LIBRARY_MARKER_NAME}</span> inside{" "}
          <span className="mono">{confirmRoot}</span>.
        </p>
        <p>
          Commander uses this marker to confirm your NAS is actually mounted before
          it ever reconciles or deletes files, so a root without it is left untouched.
          Leave the file in place — deleting it pauses reconciliation for this root
          until you re-link again.
        </p>
      </Dialog>
    </div>
  );
}

/* ---- Plate solving: star catalogs -------------------------------------- */

/** One-line coverage blurb per catalog; falls back to the raw name if unknown. */
const CATALOG_COVERAGE: Record<string, string> = {
  d05: "wide fields (≳0.6°)",
  d20: "≳0.3°",
  d50: "≳0.2°",
};

function catalogPct(got?: number, total?: number): number {
  if (!total || total <= 0) return 0;
  return Math.min(100, Math.max(0, Math.round((got ?? 0) / total * 100)));
}

/**
 * Read-only star-catalog manager driven by the LIVE snapshot's solver_status —
 * NOT the editable config working copy. Each catalog shows its coverage and pinned
 * size and either an "Installed" marker or a Download button; a download in flight
 * shows live progress from the snapshot. Kept out of the config save flow entirely.
 */
function CatalogSection({ solver }: { solver?: ProvisionStatus }) {
  const toast = useToast();
  // The catalog whose download POST is in flight (before the snapshot reflects it).
  const [starting, setStarting] = useState<string | null>(null);
  const [resolving, setResolving] = useState(false);

  const resolveFailed = useCallback(async () => {
    setResolving(true);
    try {
      const { requeued } = await api.resolveFailed();
      toast.success(
        requeued > 0
          ? `Re-queued ${requeued} failed frame${requeued === 1 ? "" : "s"} for solving.`
          : "No failed frames to re-solve.",
      );
    } catch (err) {
      toast.error(
        err instanceof ApiError ? err.message : "Couldn't reach the server to re-solve frames.",
      );
    } finally {
      setResolving(false);
    }
  }, [toast]);

  const download = useCallback(
    async (name: string) => {
      setStarting(name);
      try {
        await api.downloadCatalog(name);
      } catch (err) {
        if (err instanceof ApiError && err.status === 409) {
          toast.error("Another catalog is already downloading. Try again once it finishes.");
        } else if (err instanceof ApiError && err.status === 503) {
          toast.error("Plate solving is off. Turn it on and restart, then download catalogs.");
        } else if (err instanceof ApiError) {
          toast.error(err.message);
        } else {
          toast.error("Couldn't reach the server to start the download.");
        }
      } finally {
        setStarting(null);
      }
    },
    [toast],
  );

  return (
    <div className="stack" style={{ ["--stack-gap" as string]: "var(--space-3)" }}>
      <h3 className="aac-field__label" style={{ margin: 0 }}>
        Star catalogs
      </h3>

      {solver && solver.state !== "ready" && solver.state !== "disabled" && (
        solver.state === "failed" ? (
          <p className="notice tone-problem" role="alert">
            <Icon name="problem" size={18} />
            <span>{solver.detail || "Star catalog unavailable — plate solving paused."}</span>
          </p>
        ) : solver.state === "provisioning" && solver.downloading ? (
          <p className="notice tone-working">
            <Icon name="alert" size={18} />
            <span>
              {(solver.got_bytes ?? 0) > 0
                ? `Downloading the ${solver.downloading} star catalog… ${catalogPct(solver.got_bytes, solver.total_bytes)}%`
                : solver.detail ||
                  `Preparing to download the ${solver.downloading} star catalog…`}
            </span>
          </p>
        ) : null
      )}

      {!solver ? (
        <p className="text-sm text-muted">Catalog status is unavailable right now.</p>
      ) : solver.catalogs.length === 0 ? (
        <p className="text-sm text-muted">No catalogs are configured.</p>
      ) : (
        <div className="catalog-list">
          {solver.catalogs.map((cat) => {
            const downloading = solver.downloading === cat.name;
            const pct = catalogPct(solver.got_bytes, solver.total_bytes);
            // Queued: chosen but no bytes yet (waiting for the current solve to
            // release the gate). Show "Queued…" rather than a misleading "0%".
            const active = downloading && (solver.got_bytes ?? 0) > 0;
            return (
              <div className="catalog-row" key={cat.name}>
                <span className="catalog-row__meta">
                  <span className="catalog-row__name">{cat.name}</span>
                  <span className="catalog-row__coverage">
                    {CATALOG_COVERAGE[cat.name] ?? cat.name} · {formatBytes(cat.size_bytes)}
                  </span>
                </span>
                <span className="catalog-row__action">
                  {cat.installed ? (
                    <span className="catalog-installed">Installed ✓</span>
                  ) : active ? (
                    <>
                      <div
                        className="catalog-bar"
                        role="progressbar"
                        aria-valuenow={pct}
                        aria-valuemin={0}
                        aria-valuemax={100}
                        aria-label={`Downloading ${cat.name}, ${pct}% complete`}
                      >
                        <div className="catalog-bar__fill" style={{ width: `${pct}%` }} />
                      </div>
                      <Button size="sm" disabled>
                        Downloading… {pct}%
                      </Button>
                    </>
                  ) : downloading ? (
                    <Button size="sm" disabled>
                      Queued…
                    </Button>
                  ) : (
                    <Button
                      size="sm"
                      loading={starting === cat.name}
                      disabled={!!solver.downloading}
                      onClick={() => download(cat.name)}
                    >
                      Download
                    </Button>
                  )}
                </span>
              </div>
            );
          })}
        </div>
      )}

      <div className="cluster" style={{ ["--cluster-gap" as string]: "var(--space-3)" }}>
        <Button size="sm" variant="secondary" loading={resolving} onClick={resolveFailed}>
          Re-solve failed frames
        </Button>
        <span className="text-sm text-muted">
          Retry frames that couldn't be solved — e.g. after installing a deeper catalog.
        </span>
      </div>
    </div>
  );
}

/* ---- Diagnostics (CPU profile capture) ---------------------------------- */

const PROFILE_DURATIONS: SelectOption[] = [
  { value: "5", label: "5 seconds" },
  { value: "15", label: "15 seconds" },
  { value: "30", label: "30 seconds" },
  { value: "60", label: "60 seconds" },
];

/**
 * Captures a CPU profile via the guarded POST /api/diagnostics/pprof. The request
 * blocks for the chosen number of seconds and returns an octet-stream, so it is
 * fetched directly (not through the JSON api client) with credentials and the
 * double-submit CSRF header, then streamed to a download via a temporary anchor —
 * a plain link/form navigation can't attach the CSRF header.
 */
function DiagnosticsSection() {
  const [seconds, setSeconds] = useState("30");
  const [capturing, setCapturing] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const capture = useCallback(async () => {
    setError(null);
    setCapturing(true);
    try {
      const res = await fetch(`/api/diagnostics/pprof?seconds=${seconds}`, {
        method: "POST",
        credentials: "same-origin",
        headers: { [CSRF_HEADER]: readCsrfToken() },
      });
      if (!res.ok) {
        setError(await profileErrorMessage(res));
        return;
      }
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      try {
        const a = document.createElement("a");
        a.href = url;
        a.download = `cpu-${profileTimestamp()}.pprof`;
        document.body.appendChild(a);
        a.click();
        a.remove();
      } finally {
        URL.revokeObjectURL(url);
      }
    } catch {
      setError("Couldn't reach the server to capture a profile.");
    } finally {
      setCapturing(false);
    }
  }, [seconds]);

  return (
    <div className="stack" style={{ ["--stack-gap" as string]: "var(--space-4)" }}>
      <Field
        label="Profile duration"
        help="How long the server records CPU activity. It stays responsive while profiling, but the download waits until this time elapses."
      >
        {(f) => (
          <Select
            {...f}
            options={PROFILE_DURATIONS}
            value={seconds}
            disabled={capturing}
            onChange={(e) => setSeconds(e.target.value)}
          />
        )}
      </Field>

      <div>
        <Button variant="primary" loading={capturing} onClick={capture}>
          {capturing ? "Capturing…" : "Capture CPU profile"}
        </Button>
      </div>

      {error && (
        <p className="notice tone-problem" role="alert">
          <Icon name="problem" size={18} />
          <span>{error}</span>
        </p>
      )}
    </div>
  );
}

async function profileErrorMessage(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { message?: string; error?: string };
    return (
      body.message ||
      body.error ||
      `Couldn't capture a profile (${res.status} ${res.statusText}).`
    );
  } catch {
    return `Couldn't capture a profile (${res.status} ${res.statusText}).`;
  }
}

/** Compact local timestamp (YYYYMMDD-HHMMSS) for the downloaded profile's name. */
function profileTimestamp(): string {
  const d = new Date();
  const p = (n: number) => String(n).padStart(2, "0");
  return (
    `${d.getFullYear()}${p(d.getMonth() + 1)}${p(d.getDate())}` +
    `-${p(d.getHours())}${p(d.getMinutes())}${p(d.getSeconds())}`
  );
}

/* ---- Key/value table (aliases, sensor map) ------------------------------ */

function KVTable({
  rows,
  onChange,
  keyLabel,
  valueLabel,
  keyPlaceholder,
  valuePlaceholder,
}: {
  rows: [string, string][];
  onChange: (rows: [string, string][]) => void;
  keyLabel: string;
  valueLabel: string;
  keyPlaceholder: string;
  valuePlaceholder: string;
}) {
  const setKey = (i: number, k: string) =>
    onChange(rows.map((r, idx) => (idx === i ? [k, r[1]] : r)));
  const setVal = (i: number, v: string) =>
    onChange(rows.map((r, idx) => (idx === i ? [r[0], v] : r)));
  const remove = (i: number) => onChange(rows.filter((_, idx) => idx !== i));
  const add = () => onChange([...rows, ["", ""]]);

  return (
    <div className="stack" style={{ ["--stack-gap" as string]: "var(--space-3)" }}>
      <div className="kv-rows">
        {rows.map((r, i) => (
          <div className="kv-row" key={i}>
            <TextInput
              mono
              aria-label={`${keyLabel} ${i + 1}`}
              placeholder={keyPlaceholder}
              value={r[0]}
              onChange={(e) => setKey(i, e.target.value)}
            />
            <Icon name="chevron-right" size={16} className="kv-row__arrow" aria-hidden />
            <TextInput
              mono
              aria-label={`${valueLabel} ${i + 1}`}
              placeholder={valuePlaceholder}
              value={r[1]}
              onChange={(e) => setVal(i, e.target.value)}
            />
            <IconButton
              label={`Remove row ${i + 1}`}
              icon={<Icon name="trash" size={16} />}
              onClick={() => remove(i)}
            />
          </div>
        ))}
        {rows.length === 0 && (
          <p className="text-sm text-muted">No rows yet.</p>
        )}
      </div>
      <div>
        <Button size="sm" icon={<Icon name="add" size={16} />} onClick={add}>
          Add row
        </Button>
      </div>
    </div>
  );
}

/* ---- Template editor ---------------------------------------------------- */

const CAL_TYPES = ["DARK", "FLAT", "BIAS"] as const;

/** Which template a field edits — every token list is scoped by this. */
type TemplateKind = "light" | "mosaic" | (typeof CAL_TYPES)[number];

const LIGHT_TOKENS = [
  "<TARGET>", "<OTA>", "<SESSION>", "<n>", "<SENSOR>", "<DATE>", "<YEAR>",
  "<MONTH>", "<DAY>", "<SET-TEMP>", "<EXPOSURE>", "<FILTER>", "<GAIN>",
  "<OFFSET>", "<BINNING>", "<RIG>",
];

/**
 * The design routing-and-filing §3 token spellings offered as chips per template
 * kind. The mosaic template is the only one that may name the panel tokens, so
 * scoping is what keeps them out of the light and calibration fields.
 */
const OFFERED_TOKENS: Record<TemplateKind, string[]> = {
  light: LIGHT_TOKENS,
  mosaic: ["<TARGET_BASE>", "<PANEL>", ...LIGHT_TOKENS],
  DARK: ["<SENSOR>", "<OTA>", "<GAIN>", "<OFFSET>", "<BINNING>", "<SET-TEMP>", "<EXPOSURE>", "<DATE>", "<YEAR>", "<MONTH>", "<DAY>", "<RIG>"],
  FLAT: ["<SENSOR>", "<OTA>", "<FILTER>", "<GAIN>", "<OFFSET>", "<BINNING>", "<DATE>", "<YEAR>", "<MONTH>", "<DAY>", "<RIG>"],
  BIAS: ["<SENSOR>", "<OTA>", "<GAIN>", "<OFFSET>", "<BINNING>", "<SET-TEMP>", "<DATE>", "<YEAR>", "<MONTH>", "<DAY>", "<RIG>"],
};

/**
 * What each kind validates against — wider than the chips only for the mosaic
 * template. <PANEL_ROW>/<PANEL_COL> are legal and save fine, since a folder per
 * row and column is a reasonable layout to ask for, but they render parsed
 * integers where <PANEL> reproduces the target's suffix verbatim (a pane named
 * 01-02 comes back as 1 and 2), so they aren't offered as a first move.
 */
const ACCEPTED_TOKENS: Record<TemplateKind, string[]> = {
  ...OFFERED_TOKENS,
  mosaic: [...OFFERED_TOKENS.mosaic, "<PANEL_ROW>", "<PANEL_COL>"],
};

/** Every token routing resolves anywhere — the "is this a variable at all"
 * universe behind the two different error messages below. */
const ALL_TOKENS = Array.from(
  new Set(Object.values(ACCEPTED_TOKENS).flat()),
);

/**
 * What each variable stands for, shown on hover and to screen readers. Most read
 * plainly enough from the name, but several do not: the observing date is
 * noon-to-noon rather than calendar, the night number never renumbers, <FILTER>
 * is the one variable allowed to vanish, and the panel variables only mean
 * anything for a mosaic. Documenting all of them beats documenting the subset
 * that happens to look confusing.
 */
const TOKEN_HELP: Record<string, string> = {
  "<TARGET>": "The frame's OBJECT name, cleaned up for use as a folder — e.g. NGC7000.",
  "<TARGET_BASE>":
    "Mosaic only. The target with its panel suffix removed, so every panel of one mosaic shares a parent — Sadr_1-1 gives Sadr.",
  "<PANEL>":
    "Mosaic only. The panel suffix exactly as the ASIAIR wrote it, zero-padding and all — Sadr_1-1 gives 1-1.",
  "<PANEL_ROW>": "Mosaic only. The panel's row number as an integer — Sadr_1-1 gives 1, and Sadr_01-02 also gives 1.",
  "<PANEL_COL>": "Mosaic only. The panel's column number as an integer — Sadr_1-1 gives 1, and Sadr_01-02 gives 2.",
  "<OTA>": "The telescope name from this rig's profile — e.g. FRA300.",
  "<SESSION>": "The session label from this rig's profile — e.g. Summer2026.",
  "<RIG>": "The rig's id from its profile — useful when several rigs share one destination.",
  "<n>":
    "Which night of this target this is, counting from 1. Assigned once and never renumbered, so an existing folder keeps its number.",
  "<SENSOR>": "The camera's short name from the sensor map — e.g. 2600MM. A camera missing from that map quarantines the frame.",
  "<DATE>":
    "The observing date in your chosen date format. It runs noon to noon, so a frame shot at 2am carries the previous day's date.",
  "<YEAR>": "The year of the same noon-to-noon observing date — e.g. 2026.",
  "<MONTH>": "The month of the same noon-to-noon observing date, zero-padded — e.g. 07.",
  "<DAY>": "The day of the same noon-to-noon observing date, zero-padded — e.g. 24.",
  "<SET-TEMP>":
    "The cooler's set-point, not the temperature actually reached — e.g. -10C. A frame shot with the cooler off has no set-point and files under uncooled.",
  "<EXPOSURE>": "Exposure time, trailing zeros trimmed — e.g. 600s, or 0.001s.",
  "<FILTER>":
    "The filter name — e.g. HA. The only variable allowed to be missing: on a one-shot-colour camera it disappears and takes one neighbouring separator with it.",
  "<GAIN>": "Gain, with its name attached — e.g. gain100.",
  "<OFFSET>": "Offset, with its name attached — e.g. offset50.",
  "<BINNING>": "Binning — e.g. Bin1, or Bin2x3 when the axes differ.",
};

function TemplateEditor({
  templates,
  stripTargetSpaces,
  mosaicDetection,
  mosaicWasEnabled,
  onMosaicDetectionChange,
  dateFormat,
  pathSep,
  onChange,
}: {
  templates: Templates;
  stripTargetSpaces: boolean;
  /** Owned here rather than with the other global switches: its only effect is
   * to decide which template files a frame, and a switch that lives a card away
   * from the field it governs makes the reader scroll to connect the two. */
  mosaicDetection: boolean;
  /** The STORED value, so the "already-filed panels stay put" note appears only
   * for an install that actually had detection on — not for one that has never
   * used it. */
  mosaicWasEnabled: boolean;
  onMosaicDetectionChange: (v: boolean) => void;
  dateFormat: string;
  pathSep: "/" | "\\";
  onChange: (t: Templates) => void;
}) {
  const [preview, setPreview] = useState<PreviewResponse | null>(null);
  const [previewState, setPreviewState] = useState<"idle" | "loading" | "unavailable" | "error">("idle");

  const setLight = (v: string) => onChange({ ...templates, light: v });
  const setMosaic = (v: string) => onChange({ ...templates, mosaic: v });
  const setCal = (type: string, v: string) =>
    onChange({ ...templates, cal: { ...templates.cal, [type]: v } });

  // Debounced live preview. The endpoint may be absent (older backend) — degrade
  // to a quiet notice rather than a hard failure.
  useEffect(() => {
    const controller = new AbortController();
    setPreviewState("loading");
    const timer = window.setTimeout(async () => {
      try {
        const res = await api.previewConfig(
          {
            templates,
            strip_target_spaces: stripTargetSpaces,
            mosaic_detection: mosaicDetection,
            date_format: dateFormat,
          },
          controller.signal,
        );
        setPreview(res);
        setPreviewState("idle");
      } catch (err) {
        if (controller.signal.aborted) return;
        if (err instanceof ApiError && err.status === 404) setPreviewState("unavailable");
        else setPreviewState("error");
      }
    }, 400);
    return () => {
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [templates, stripTargetSpaces, mosaicDetection, dateFormat]);

  return (
    <div className="stack" style={{ ["--stack-gap" as string]: "var(--space-4)" }}>
      <Field
        label="Detect and sort mosaic panels"
        help="The ASIAIR's mosaic planner names each pane <target>_<row>-<col> — Sadr_1-1, Sadr_1-2 — so without this every panel lands as its own top-level target folder. With it on, a light frame whose target reads that way is filed by the mosaic template below instead, which puts a mosaic's panels under one parent. Panels are recognised by that naming alone, so a target of your own that happens to share the shape (Abell_2-1) is treated as a panel too."
        example="'Sadr_1-1' files under 'Sadr'"
      >
        {() => (
          <Toggle
            checked={mosaicDetection}
            onChange={onMosaicDetectionChange}
            label={mosaicDetection ? "On" : "Off"}
          />
        )}
      </Field>
      {mosaicWasEnabled && !mosaicDetection && (
        <p className="notice tone-neutral">
          <Icon name="info" size={18} />
          <span>
            Panels already filed under the mosaic layout will stay where they are.
            Turn this back on if you want to move them.
          </span>
        </p>
      )}

      <TemplateField
        label="Light frames"
        help="Built under the light-frames root. Must include the night token <n>."
        value={templates.light}
        tokens={OFFERED_TOKENS.light}
        pathSep={pathSep}
        onChange={setLight}
      />
      {/* Shown even while detection is off, read-only, so the layout it would
          produce is discoverable rather than hidden behind a switch — and so
          toggling does not make the surrounding fields jump. */}
      <TemplateField
        label="Mosaic panels"
        help="Built under the light-frames root, for a light frame whose target names a mosaic panel. Must include the night token <n>, and must tell the panels apart before it — use <PANEL> or <TARGET> somewhere above the night folder, or every panel of a mosaic lands in one folder."
        value={templates.mosaic}
        tokens={OFFERED_TOKENS.mosaic}
        accepted={ACCEPTED_TOKENS.mosaic}
        pathSep={pathSep}
        onChange={setMosaic}
        disabled={!mosaicDetection}
        disabledNote="Inactive — turn on mosaic panel detection above to use this."
      />
      {CAL_TYPES.map((type) => (
        <TemplateField
          key={type}
          label={`Calibration — ${calLabel(type)}`}
          help="Built under the calibration-frames root."
          value={templates.cal[type] ?? ""}
          tokens={OFFERED_TOKENS[type]}
          pathSep={pathSep}
          onChange={(v) => setCal(type, v)}
        />
      ))}

      <div>
        <h3 className="aac-field__label" style={{ marginBottom: "var(--space-2)" }}>
          Live preview
        </h3>
        {previewState === "unavailable" ? (
          <p className="notice tone-neutral">
            <Icon name="info" size={18} />
            <span>
              Live preview isn't available on this server. Your templates still
              save and apply normally.
            </span>
          </p>
        ) : previewState === "error" ? (
          <p className="notice tone-neutral">
            <Icon name="info" size={18} />
            <span>Couldn't build a preview right now. Editing still works.</span>
          </p>
        ) : previewState === "loading" && !preview ? (
          <Spinner label="Building preview…" />
        ) : preview && preview.samples.length > 0 ? (
          <div className="preview">
            {preview.samples.map((s, i) => (
              <div className="preview__row" key={i}>
                <span className="preview__type">
                  {s.type}
                  {s.synthetic ? " · example frame" : ""}
                </span>
                <span className="preview__path">from {s.source}</span>
                {s.error ? (
                  <span className="preview__error">
                    <Icon name="problem" size={15} />
                    <span>{s.error}</span>
                  </span>
                ) : (
                  <span className="preview__path preview__dest">→ {s.dest}</span>
                )}
              </div>
            ))}
          </div>
        ) : (
          <p className="text-sm text-muted">
            No recent frames to preview yet. Once frames have been pulled, this
            shows where each type would land.
          </p>
        )}
      </div>
    </div>
  );
}

function TemplateField({
  label,
  help,
  value,
  tokens,
  accepted,
  pathSep,
  onChange,
  disabled = false,
  disabledNote,
}: {
  label: string;
  help: string;
  value: string;
  /** The chips offered for insertion. */
  tokens: string[];
  /** What the field validates against, when that is wider than the chips — a
   * token can be legal without being advertised. Defaults to the chips. */
  accepted?: string[];
  pathSep: "/" | "\\";
  onChange: (v: string) => void;
  /** Shows the template but refuses edits, for one whose feature is switched
   * off. Visible-and-inert beats hidden: the operator can see what the layout
   * would be before committing to it, and nothing shifts when the switch moves.
   * It also keeps the field unable to go invalid while the server suppresses its
   * validation. */
  disabled?: boolean;
  disabledNote?: string;
}) {
  const [editing, setEditing] = useState(false);
  const ref = useRef<HTMLInputElement>(null);
  const pendingCaret = useRef<number | null>(null);

  // Templates are stored canonically with "/", but an operator on Windows expects
  // to see and type "\". Present the platform separator and canonicalize any "\"
  // the operator types straight back to "/", so component state (and the saved
  // config) stays "/"-joined while the field reads native. Separators are single
  // characters, so this 1:1 swap never shifts caret math in the input.
  const display = pathSep === "/" ? value : value.replaceAll("/", pathSep);
  const canonicalize = (v: string) => v.replaceAll("\\", "/");

  // Restore the caret after a chip insertion re-renders the controlled input.
  useLayoutEffect(() => {
    if (pendingCaret.current != null && ref.current) {
      const pos = pendingCaret.current;
      ref.current.focus();
      ref.current.setSelectionRange(pos, pos);
      pendingCaret.current = null;
    }
  });

  const insert = useCallback(
    (token: string) => {
      const el = ref.current;
      const start = el ? el.selectionStart ?? value.length : value.length;
      const end = el ? el.selectionEnd ?? start : start;
      const next = value.slice(0, start) + token + value.slice(end);
      pendingCaret.current = start + token.length;
      onChange(next);
    },
    [value, onChange],
  );

  // Validate every <…> against the variables allowed for THIS template. A typo is
  // "not a valid variable name"; a real variable that's just out of scope (e.g.
  // <n> in a calibration template) says so. An invalid template blocks Done.
  const used = value.match(/<[^>]*>/g) ?? [];
  const allowed = new Set(accepted ?? tokens);
  const badToken = used.find((t) => !allowed.has(t));
  const tokenError = !badToken
    ? null
    : ALL_TOKENS.includes(badToken)
      ? `${badToken} isn't available for this template.`
      : `${badToken} is not a valid variable name.`;

  return (
    <div className="tmpl" data-disabled={disabled || undefined}>
      <div className="tmpl__head">
        <span className="tmpl__name">{label}</span>
        {disabled ? null : editing ? (
          <Button
            size="sm"
            variant="secondary"
            disabled={tokenError != null}
            title={tokenError ?? undefined}
            onClick={() => setEditing(false)}
          >
            Done
          </Button>
        ) : (
          <Button size="sm" variant="ghost" icon={<Icon name="edit" size={16} />} onClick={() => setEditing(true)}>
            Edit
          </Button>
        )}
      </div>

      {editing && !disabled ? (
        <>
          <p className="aac-field__help">{help}</p>
          <TextInput
            ref={ref}
            mono
            aria-label={`${label} template`}
            value={display}
            onChange={(e) => onChange(canonicalize(e.target.value))}
          />
          {tokenError && (
            <p
              role="alert"
              style={{
                color: "var(--problem-fg)",
                fontSize: "var(--text-sm)",
                margin: "var(--space-1) 0 0",
              }}
            >
              {tokenError}
            </p>
          )}
          <div className="chip-row" role="group" aria-label={`Insert a variable into the ${label} template`}>
            {tokens.map((t) => (
              <button
                key={t}
                type="button"
                className="chip"
                onClick={() => insert(t)}
                title={TOKEN_HELP[t]}
                aria-label={TOKEN_HELP[t] ? `Insert ${t} — ${TOKEN_HELP[t]}` : `Insert ${t}`}
              >
                {t}
              </button>
            ))}
          </div>
        </>
      ) : (
        <>
          <span className="tmpl__value">{display || "(empty)"}</span>
          {disabled && disabledNote && (
            <p className="aac-field__help tmpl__note">{disabledNote}</p>
          )}
        </>
      )}
    </div>
  );
}

/* ---- helpers ------------------------------------------------------------ */

/**
 * Infers the host's path separator from the operator-typed destination roots
 * (native strings the backend round-trips verbatim): a "\" anywhere means
 * Windows. Avoids a dedicated platform endpoint — the roots already carry the
 * signal. Falls back to "/" before any root is set.
 */
function pathSeparator(roots: Roots): "/" | "\\" {
  const sample = roots.light || roots.cal || roots.staging || roots.logs || "";
  return sample.includes("\\") ? "\\" : "/";
}

function calLabel(type: string): string {
  return type === "DARK" ? "Darks" : type === "FLAT" ? "Flats" : type === "BIAS" ? "Bias" : type;
}

function rowsToObject(rows: [string, string][]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of rows) {
    const key = k.trim();
    if (key) out[key] = v.trim();
  }
  return out;
}

function parseCsv(s: string): string[] {
  return s
    .split(/[\n,]/)
    .map((x) => x.trim())
    .filter(Boolean);
}

function toInt(s: string, fallback: number): number {
  const n = Number(s);
  return Number.isFinite(n) && s.trim() !== "" ? Math.trunc(n) : fallback;
}

function round2(n: number): number {
  return Math.round(n * 100) / 100;
}
