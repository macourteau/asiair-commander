/**
 * Setup — the first-run wizard, shown when no password is set. It runs from any
 * browser, and the backend requires the one-time token Commander printed at
 * startup on every attempt — loopback included — so the token is the first field
 * of the form rather than a recovery flow bolted onto a 403.
 *
 * Collects what is needed to start: the token, a password, the addresses
 * Commander will answer to, the destination roots, and (optionally) a first rig.
 * Everything else is tunable later in Settings/Devices.
 */
import { useEffect, useRef, useState, type FormEvent } from "react";
import { Navigate, useNavigate } from "react-router";
import {
  api,
  ApiError,
  SETUP_COMPLETE,
  SETUP_TOKEN_REQUIRED,
  type DiscoveredRig,
  type PlatformCategory,
  type Problem,
  type RootCheckResult,
} from "@/api";
import { Button, Card, Field, Icon, IconButton, Spinner, TextInput } from "@/design";
import { AllowedHostsField } from "@/components/AllowedHostsField";
import { setupTokenFromUrl } from "../auth/setupToken";
import { RootsUnwritableError, useAuth, type SetupPayload } from "../auth/AuthContext";
import { defaultRoots, SETUP_LANDING } from "../util/setupDefaults";

/** deviceLabel derives a friendly name from a discovered device's model + guid. */
function deviceLabel(d: DiscoveredRig): string {
  const short = d.guid ? d.guid.slice(0, 8) : "unknown";
  return d.model ? `${d.model} (${short})` : `ASIAIR ${short}`;
}

/**
 * Where to look for the token, worded for the platform the server reported. The
 * channel differs per platform and a wrong answer sends the operator to a log
 * that does not exist for them, so the server tells us which one to name.
 */
function tokenLocation(platform: PlatformCategory | undefined) {
  if (platform === "container") {
    return (
      <>
        Find it with <code>docker logs &lt;container&gt;</code>.
      </>
    );
  }
  if (platform === "kubernetes") {
    return (
      <>
        Find it with <code>kubectl logs &lt;pod&gt;</code>.
      </>
    );
  }
  return (
    <>
      Find it in the log file in Commander's data folder, or in the{" "}
      <code>setup-token</code> file beside it. On Windows both are readable only by
      an administrator.
    </>
  );
}

export function Setup() {
  const { status, completeSetup } = useAuth();
  const navigate = useNavigate();

  // Seeded from the URL fragment the Windows launcher opens Commander with; empty
  // everywhere else, where the operator pastes it. Kept in state only — never in
  // storage, since it would outlive the window it is good for.
  const [token, setToken] = useState(setupTokenFromUrl);
  const [tokenHelpOpen, setTokenHelpOpen] = useState(false);
  const [platform, setPlatform] = useState<PlatformCategory | undefined>();

  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  // The addresses Commander will answer to. Pre-filled with the one this browser
  // is using, which is the one that must keep working after setup.
  const [allowedHosts, setAllowedHosts] = useState<string[]>(() => [
    window.location.hostname,
  ]);
  const [hostProblems, setHostProblems] = useState<Problem[]>([]);
  const [roots, setRoots] = useState({ light: "", cal: "", staging: "", logs: "" });
  // Set once the server's platform has real default folders, which then stand in
  // for the example paths.
  const [hasDefaults, setHasDefaults] = useState(false);
  // ASIAIR file sharing uses well-known guest credentials, so pre-fill them.
  const [rig, setRig] = useState({ name: "", guid: "", cpuId: "", user: "guest", password: "12345678" });

  // Device picker: scan the network so the operator can adopt a rig by picking it
  // (binding is by the device's stable id, not a hand-typed address).
  const [devices, setDevices] = useState<DiscoveredRig[] | null>(null);
  const [scanning, setScanning] = useState(false);
  const scan = () => {
    setScanning(true);
    api
      .scan()
      .then((found) => setDevices(found.filter((d) => !d.managed)))
      .catch(() => setDevices([]))
      .finally(() => setScanning(false));
  };
  useEffect(() => {
    scan();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Which channel to point the operator at for the token, and which default
  // folders to pre-fill. A failed probe just leaves the generic wording, which is
  // correct everywhere, only less specific, and empty folders to type in.
  useEffect(() => {
    const ac = new AbortController();
    api
      .getSetupStatus(ac.signal)
      .then((s) => {
        setPlatform(s.platform);
        const d = defaultRoots(s.os);
        if (!d) return;
        setHasDefaults(true);
        // Never overwrite what the operator typed before the probe answered.
        setRoots((r) => ({ ...r, light: r.light || d.light, staging: r.staging || d.staging }));
      })
      .catch(() => undefined);
    return () => ac.abort();
  }, []);

  const [error, setError] = useState<string | null>(null);
  const [fieldErr, setFieldErr] = useState<Record<string, string>>({});
  const [submitting, setSubmitting] = useState(false);
  // Set when the service account cannot write one or more destination roots: the
  // wizard blocks completion and shows per-path guidance with a Retry.
  const [rootBlock, setRootBlock] = useState<{ account: string; failures: RootCheckResult[] } | null>(
    null,
  );
  // Once the password has been seeded (setup POST succeeded), a folder-permission
  // block leaves the operator on the wizard to fix roots and retry. The password
  // is already saved and the retry does NOT re-send it, so the fields are locked
  // to avoid silently discarding an edit and locking the operator out.
  const [seeded, setSeeded] = useState(false);
  const rootAlertRef = useRef<HTMLDivElement>(null);
  // Where to land once completion flips the status to authenticated. Completing
  // setup sets that status from inside completeSetup, so the guard below re-renders
  // and its redirect wins over the navigate that follows it — the guard therefore
  // has to know the wizard was on its way to SETUP_LANDING.
  const completed = useRef(false);

  // Move focus to the permission guidance when it appears, so keyboard and
  // screen-reader users land on what they must act on.
  useEffect(() => {
    if (rootBlock) rootAlertRef.current?.focus();
  }, [rootBlock]);

  if (status === "authenticated")
    return <Navigate to={completed.current ? SETUP_LANDING : "/"} replace />;
  if (status === "unauthenticated") return <Navigate to="/login" replace />;

  function validate(): boolean {
    const errs: Record<string, string> = {};
    // Required here as well as server-side, so a missing token costs no round trip.
    if (!token.trim()) errs.token = "Paste the token Commander printed when it started.";
    if (password.length < 8) errs.password = "Use at least 8 characters.";
    if (confirm !== password) errs.confirm = "The two passwords don't match.";
    if (!roots.light.trim()) errs.light = "Commander needs somewhere to file light frames.";
    if (!roots.staging.trim()) errs.staging = "Commander needs a temporary folder for files while they copy.";
    setFieldErr(errs);
    return Object.keys(errs).length === 0;
  }

  async function runSetup() {
    setError(null);
    setRootBlock(null);
    setHostProblems([]);
    if (!validate()) return;
    setSubmitting(true);

    const payload: SetupPayload = {
      password,
      token: token.trim(),
      allowedHosts: allowedHosts.map((h) => h.trim()).filter(Boolean),
      roots: {
        light: roots.light.trim(),
        cal: roots.cal.trim() || roots.light.trim(),
        staging: roots.staging.trim(),
        logs: roots.logs.trim(),
      },
    };
    if (rig.guid && rig.name.trim()) {
      payload.rig = {
        name: rig.name.trim(),
        guid: rig.guid,
        cpuId: rig.cpuId,
        user: rig.user.trim(),
        password: rig.password,
        managed: true,
      };
    }

    try {
      completed.current = true;
      await completeSetup(payload);
      navigate(SETUP_LANDING, { replace: true });
    } catch (err) {
      if (err instanceof RootsUnwritableError) {
        setSeeded(true); // the password POST already succeeded before the roots check
        setRootBlock({ account: err.account, failures: err.failures });
      } else if (err instanceof ApiError && err.code === SETUP_COMPLETE) {
        // Reachable without any attack: a lost response and a retry, a second tab,
        // a restart between the three calls. The wizard is dead either way, so send
        // the operator somewhere that works instead of stranding them here.
        navigate("/login", { replace: true });
        return;
      } else if (err instanceof ApiError && err.code === SETUP_TOKEN_REQUIRED) {
        // Put the correction on the field the operator has to fix, not in a banner.
        setFieldErr((e) => ({
          ...e,
          token:
            "That token doesn't match. If Commander has restarted since you copied it, check again for the current one.",
        }));
      } else if (err instanceof ApiError && err.problems) {
        const hosts = err.problems.filter((p) => p.Field === "web.allowed_hosts");
        setHostProblems(hosts);
        if (hosts.length < err.problems.length) setError(err.message);
      } else if (err instanceof ApiError) {
        setError(err.message);
      } else {
        setError("Couldn't reach the server. Check that Commander is running on the NAS.");
      }
      setSubmitting(false);
    }
  }

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    await runSetup();
  }

  return (
    <main className="auth-screen">
      <div className="auth-brand">
        <Icon name="rig" size={26} className="auth-brand__mark" />
        <span className="auth-wordmark">
          ASIAIR Commander
          <small>First-time setup</small>
        </span>
      </div>

      <Card as="form" pad className="auth-card auth-card--wide" onSubmit={onSubmit}>
        <h1 className="auth-title">Set up Commander</h1>
        <p className="auth-lede">
          A few basics to get started. You can change any of this later in Settings.
        </p>

        <p className="auth-note" style={{ marginBottom: "var(--space-5)" }}>
          <Icon name="info" size={18} />
          <span>
            You can finish setup from any browser. It needs the one-time token Commander
            printed when it started, which is how it knows you can reach the machine it
            runs on.
          </span>
        </p>

        {error && (
          <p className="auth-formerror" role="alert" style={{ marginBottom: "var(--space-5)" }}>
            <Icon name="problem" size={18} />
            <span>{error}</span>
          </p>
        )}

        {rootBlock && (
          <div
            ref={rootAlertRef}
            className="auth-formerror"
            role="alert"
            tabIndex={-1}
            style={{ marginBottom: "var(--space-5)", flexDirection: "column", alignItems: "stretch" }}
          >
            <p style={{ display: "flex", gap: "var(--space-2)", alignItems: "flex-start", margin: 0 }}>
              <Icon name="problem" size={18} />
              <span>
                Commander runs as the account <code>{rootBlock.account}</code>, which can't write
                these folders yet. Grant it <strong>Modify</strong> access, then retry.
              </span>
            </p>
            <ul style={{ margin: "var(--space-3) 0 0", paddingLeft: "var(--space-6)" }}>
              {rootBlock.failures.map((f) => (
                <li key={f.path} style={{ marginBottom: "var(--space-2)" }}>
                  <code>{f.path}</code>
                  {f.error ? <span className="text-muted text-sm"> — {f.error}</span> : null}
                </li>
              ))}
            </ul>
            <div style={{ marginTop: "var(--space-3)" }}>
              <Button type="button" variant="secondary" onClick={() => void runSetup()} loading={submitting}>
                Retry
              </Button>
            </div>
          </div>
        )}

        <div className="stack" style={{ ["--stack-gap" as string]: "var(--space-5)" }}>
          <fieldset className="auth-section stack" style={{ ["--stack-gap" as string]: "var(--space-4)" }}>
            <legend>Setup token</legend>
            <div
              className="cluster"
              style={{
                ["--cluster-gap" as string]: "var(--space-2)",
                alignItems: "flex-start",
                // Keep the (?) beside the field rather than wrapping under it.
                flexWrap: "nowrap",
              }}
            >
              <div className="grow">
                <Field
                  label="Setup token"
                  required
                  help="Commander printed this when it started."
                  error={fieldErr.token}
                >
                  {(f) => (
                    <TextInput
                      {...f}
                      mono
                      name="setup_token"
                      autoComplete="off"
                      spellCheck={false}
                      value={token}
                      disabled={seeded}
                      onChange={(e) => setToken(e.target.value)}
                    />
                  )}
                </Field>
              </div>
              <IconButton
                label="Where do I find the setup token?"
                icon={<Icon name="info" size={18} />}
                active={tokenHelpOpen}
                aria-expanded={tokenHelpOpen}
                onClick={() => setTokenHelpOpen((v) => !v)}
              />
            </div>
            {tokenHelpOpen && (
              <div
                className="auth-note"
                style={{ flexDirection: "column", alignItems: "stretch", gap: "var(--space-2)" }}
              >
                <p style={{ margin: 0 }}>
                  Commander printed a one-time token when it started, and needs it to
                  confirm you can reach the machine it runs on.
                </p>
                <p style={{ margin: 0 }}>{tokenLocation(platform)}</p>
                <p style={{ margin: 0 }}>
                  The token changes every time Commander restarts, so if you copied it a
                  while ago, check again.
                </p>
              </div>
            )}
          </fieldset>

          <fieldset className="auth-section stack" style={{ ["--stack-gap" as string]: "var(--space-4)" }}>
            <legend>Password</legend>
            <Field
              label="Set a password"
              required
              help={
                seeded
                  ? "Password already saved — finish by resolving the folder access below."
                  : "You'll use this to sign in from any browser on your network."
              }
              error={fieldErr.password}
            >
              {(f) => (
                <TextInput
                  {...f}
                  type="password"
                  autoComplete="new-password"
                  value={password}
                  disabled={seeded}
                  onChange={(e) => setPassword(e.target.value)}
                />
              )}
            </Field>
            <Field label="Confirm password" required error={fieldErr.confirm}>
              {(f) => (
                <TextInput
                  {...f}
                  type="password"
                  autoComplete="new-password"
                  value={confirm}
                  disabled={seeded}
                  onChange={(e) => setConfirm(e.target.value)}
                />
              )}
            </Field>
          </fieldset>

          <fieldset className="auth-section stack" style={{ ["--stack-gap" as string]: "var(--space-4)" }}>
            <legend>Addresses you'll use to reach Commander</legend>
            <AllowedHostsField
              value={allowedHosts}
              onChange={setAllowedHosts}
              // Locked once the setup POST has succeeded: a retry after a folder
              // block resumes at the roots check and will not re-send these, so an
              // edit here would be silently discarded.
              disabled={seeded}
              label="Addresses"
              help="Commander only answers to addresses you list here, which is what stops a malicious web page from talking to it behind your back. The address you're using now is filled in. Add any other name or address you'll open Commander with — its hostname, its IP, or a domain you put in front of it."
              problems={hostProblems}
            />
          </fieldset>

          <fieldset className="auth-section stack" style={{ ["--stack-gap" as string]: "var(--space-4)" }}>
            <legend>Where files go</legend>
            <Field
              label="Light frames folder"
              required
              help="The destination for your imaging subs, sorted into per-target folders."
              example={hasDefaults ? undefined : "D:\\Astro\\Lights"}
              error={fieldErr.light}
            >
              {(f) => (
                <TextInput
                  {...f}
                  mono
                  value={roots.light}
                  onChange={(e) => setRoots((r) => ({ ...r, light: e.target.value }))}
                />
              )}
            </Field>
            <Field
              label="Calibration frames folder"
              help="Darks, flats, and bias. Leave blank to keep them with the lights."
              example={hasDefaults ? undefined : "D:\\Astro\\Calibration"}
            >
              {(f) => (
                <TextInput
                  {...f}
                  mono
                  value={roots.cal}
                  onChange={(e) => setRoots((r) => ({ ...r, cal: e.target.value }))}
                />
              )}
            </Field>
            <Field
              label="Temporary folder (same drive as your pictures)"
              required
              help="Files wait here while they copy, before they're filed."
              example={hasDefaults ? undefined : "D:\\Astro\\_staging"}
              error={fieldErr.staging}
            >
              {(f) => (
                <TextInput
                  {...f}
                  mono
                  value={roots.staging}
                  onChange={(e) => setRoots((r) => ({ ...r, staging: e.target.value }))}
                />
              )}
            </Field>
            <Field
              label="Logs folder"
              help="Where pulled device logs are kept. Can live on a different drive."
              example={hasDefaults ? undefined : "D:\\Astro\\logs"}
            >
              {(f) => (
                <TextInput
                  {...f}
                  mono
                  value={roots.logs}
                  onChange={(e) => setRoots((r) => ({ ...r, logs: e.target.value }))}
                />
              )}
            </Field>
          </fieldset>

          <fieldset className="auth-section stack" style={{ ["--stack-gap" as string]: "var(--space-4)" }}>
            <legend>Your first rig (optional)</legend>
            <div className="cluster-between">
              <p className="text-muted text-sm">
                Pick your ASIAIR from the ones found on your network, or skip and add it
                later on the Devices page.
              </p>
              <Button
                type="button"
                size="sm"
                variant="secondary"
                icon={<Icon name="scan" size={16} />}
                loading={scanning}
                onClick={scan}
              >
                Scan again
              </Button>
            </div>

            {!devices ? (
              <Spinner label="Scanning your network…" />
            ) : devices.length === 0 ? (
              <p className="text-muted text-sm">
                No ASIAIR answered yet. Power one on and Scan again, or skip and add it
                later.
              </p>
            ) : (
              <div className="stack" style={{ ["--stack-gap" as string]: "var(--space-2)" }}>
                {devices.map((d) => {
                  const selected = rig.guid === d.guid;
                  return (
                    <button
                      key={d.guid}
                      type="button"
                      className={`aac-input${selected ? " is-selected" : ""}`}
                      aria-pressed={selected}
                      style={{
                        textAlign: "left",
                        cursor: "pointer",
                        borderColor: selected ? "var(--accent, #58a6ff)" : undefined,
                      }}
                      onClick={() =>
                        setRig((r) => ({
                          ...r,
                          guid: d.guid,
                          cpuId: d.cpuId,
                          name: r.name.trim() ? r.name : deviceLabel(d),
                        }))
                      }
                    >
                      <span style={{ display: "flex", alignItems: "center", gap: "var(--space-2)" }}>
                        <span
                          aria-hidden
                          style={{
                            display: "inline-block",
                            width: 8,
                            height: 8,
                            borderRadius: "50%",
                            background: d.online
                              ? "var(--tone-safe, #3fb950)"
                              : "var(--tone-muted, #8b949e)",
                          }}
                        />
                        <span>{deviceLabel(d)}</span>
                        <span className="mono text-muted text-sm">{d.address}</span>
                      </span>
                    </button>
                  );
                })}
              </div>
            )}

            <Field label="Rig name" help="A friendly name you'll recognize." example="Backyard 8-inch">
              {(f) => (
                <TextInput
                  {...f}
                  value={rig.name}
                  onChange={(e) => setRig((r) => ({ ...r, name: e.target.value }))}
                />
              )}
            </Field>
            <div className="cluster" style={{ ["--cluster-gap" as string]: "var(--space-4)", alignItems: "flex-start" }}>
              <div className="grow">
                <Field label="Share username" help="The ASIAIR file-sharing login. Default: guest.">
                  {(f) => (
                    <TextInput
                      {...f}
                      value={rig.user}
                      onChange={(e) => setRig((r) => ({ ...r, user: e.target.value }))}
                    />
                  )}
                </Field>
              </div>
              <div className="grow">
                <Field label="Share password" help="The ASIAIR file-sharing password. Default: 12345678.">
                  {(f) => (
                    <TextInput
                      {...f}
                      autoComplete="off"
                      value={rig.password}
                      onChange={(e) => setRig((r) => ({ ...r, password: e.target.value }))}
                    />
                  )}
                </Field>
              </div>
            </div>
          </fieldset>

          <Button type="submit" variant="primary" block loading={submitting}>
            Finish setup
          </Button>
        </div>
      </Card>
    </main>
  );
}
