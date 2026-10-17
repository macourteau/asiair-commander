/**
 * Auth state for the whole SPA. On mount it runs the two-probe bootstrap to
 * decide between three states:
 *
 *   setup           — GET /api/setup → { needed: true }: no password yet → wizard
 *   unauthenticated — password set, GET /api/config → 401 → show Login
 *   authenticated   — GET /api/config → 200 → show the app
 *
 * A 401 from any API call (dispatched by the client as UNAUTHORIZED_EVENT) drops
 * us back to `unauthenticated` so the guard bounces to Login.
 */
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { api, UNAUTHORIZED_EVENT } from "@/api";
import type { Config, RigProfile, RootCheckResult, Roots } from "@/api";

/**
 * Thrown by completeSetup when the service account cannot write one or more
 * destination roots. The wizard catches it to show per-path guidance naming
 * `account`, and offers a Retry that re-runs the check without re-seeding the
 * password (which is already set by then).
 */
export class RootsUnwritableError extends Error {
  readonly account: string;
  readonly failures: RootCheckResult[];
  constructor(account: string, failures: RootCheckResult[]) {
    super("destination folders are not writable by the service account");
    this.name = "RootsUnwritableError";
    this.account = account;
    this.failures = failures;
  }
}

export type AuthStatus =
  | "loading"
  | "setup"
  | "unauthenticated"
  | "authenticated";

export interface SetupPayload {
  password: string;
  /** The one-time token from the service log; required by the server. */
  token?: string;
  /**
   * Addresses the operator will reach Commander at. They ride the setup POST, not
   * the final PUT, so a first run that changes nothing else still records them.
   */
  allowedHosts?: string[];
  rig?: Partial<RigProfile>;
  roots?: Partial<Roots>;
}

interface AuthContextValue {
  status: AuthStatus;
  /** Re-probe the bootstrap signal. */
  refresh: () => Promise<void>;
  /** Throws ApiError on a bad password / rate-limit; caller renders the message. */
  login: (password: string) => Promise<void>;
  logout: () => Promise<void>;
  completeSetup: (payload: SetupPayload) => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

/** Drop empty-string roots so the wizard never clobbers a seeded default with "". */
function nonEmptyRoots(roots?: Partial<Roots>): Partial<Roots> {
  const out: Partial<Roots> = {};
  for (const [k, v] of Object.entries(roots ?? {})) {
    if (typeof v === "string" && v.trim()) out[k as keyof Roots] = v.trim();
  }
  return out;
}

/** Turn a display name into a stable, filesystem-safe rig id. */
function slugId(name: string): string {
  const slug = name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/(^-|-$)/g, "");
  return slug || `rig-${Date.now().toString(36)}`;
}

/** Build a minimal valid first rig from the wizard fields (grace inherits defaults). */
function buildFirstRig(rig: Partial<RigProfile>): RigProfile {
  return {
    id: slugId(rig.name ?? rig.host ?? "rig"),
    name: rig.name ?? "",
    guid: rig.guid ?? "",
    cpuId: rig.cpuId ?? "",
    host: rig.host ?? "",
    user: rig.user ?? "",
    password: rig.password ?? "",
    ota: rig.ota ?? "",
    session: rig.session ?? "",
    sources: rig.sources ?? [],
    managed: rig.managed ?? true,
    capacity_constrained: rig.capacity_constrained ?? false,
    pull_logs: rig.pull_logs ?? false,
    keep_originals: rig.keep_originals ?? false,
    sync_clock: rig.sync_clock ?? false,
  };
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthStatus>("loading");

  const refresh = useCallback(async () => {
    try {
      const { needed } = await api.getSetupStatus();
      if (needed) {
        setStatus("setup");
        return;
      }
      const authed = await api.probeSession();
      setStatus(authed ? "authenticated" : "unauthenticated");
    } catch {
      // Bootstrap probe failed (backend down / not reachable). Treat as logged
      // out so the user sees Login rather than a blank screen; the Login attempt
      // surfaces the real connection error.
      setStatus("unauthenticated");
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // Read inside the 401 listener without re-registering it on every status change.
  const statusRef = useRef(status);
  statusRef.current = status;

  useEffect(() => {
    const onUnauthorized = () => {
      // NOT dead code: during first run there is by definition no session, so a
      // 401 means "not set up yet", never "your session expired". The wizard's
      // device scan hits POST /api/scan, which is session-guarded and answers 401
      // — and treating that as an expired session would bounce the operator
      // from /setup to /login, making the wizard unreachable. Only loopback
      // reaches this: a remote browser is refused earlier by the Origin check
      // with a 403, which never fires this event, so the desktop console flow
      // first-run setup is meant to keep frictionless is the one at stake.
      if (statusRef.current === "setup") return;
      setStatus("unauthenticated");
    };
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
  }, []);

  const login = useCallback(async (password: string) => {
    await api.login(password);
    setStatus("authenticated");
  }, []);

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } finally {
      setStatus("unauthenticated");
    }
  }, []);

  // The seeded config from the one-time password step, cached so a retry after a
  // roots-writability failure does NOT re-POST /api/setup (which now 403s: the
  // password is already set). Only the roots check and the final PUT re-run.
  const seededConfig = useRef<Config | null>(null);

  const completeSetup = useCallback(async (payload: SetupPayload) => {
    // The backend only seeds full defaults when no config is sent, and it can't
    // merge a partial one. So set the password first (seeding a valid default
    // config) exactly once, caching the result for retries.
    if (!seededConfig.current) {
      const { config } = await api.setup(
        {
          password: payload.password,
          ...(payload.allowedHosts?.length
            ? { allowed_hosts: payload.allowedHosts }
            : {}),
        },
        payload.token,
      );
      seededConfig.current = config;
    }
    const config = seededConfig.current;

    const rootOverrides = nonEmptyRoots(payload.roots);
    const mergedRoots = { ...config.roots, ...rootOverrides };

    // Verify the SERVICE ACCOUNT can actually write every destination before
    // committing the config. The low-privilege account can't self-grant folder
    // access, so an unwritable root must be surfaced (with the account name) and
    // fixed by the operator, then retried — never silently saved.
    const check = await api.checkRoots(mergedRoots);
    const failures = check.results.filter((r) => !r.ok);
    if (failures.length > 0) {
      throw new RootsUnwritableError(check.account, failures);
    }

    const hasRoots = Object.keys(rootOverrides).length > 0;
    const hasRig = Boolean(payload.rig?.host && payload.rig?.name);
    if (hasRoots || hasRig) {
      const next: Config = {
        ...config,
        roots: mergedRoots,
        rigs: hasRig
          ? [...(config.rigs ?? []), buildFirstRig(payload.rig!)]
          : config.rigs,
      };
      await api.putConfig(next);
    }

    setStatus("authenticated");
  }, []);

  const value = useMemo<AuthContextValue>(
    () => ({ status, refresh, login, logout, completeSetup }),
    [status, refresh, login, logout, completeSetup],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within <AuthProvider>");
  return ctx;
}
