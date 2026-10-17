/**
 * What a remote's card says: its state line, latency, build-age notice, recent
 * warnings, Commander's own relay reachability, and which rigs it reaches. Pure, so
 * the wording each state settles on is pinned by tests rather than by a browser
 * (design remote-sites §10).
 */
import type {
  DiscoveredRig,
  RelayStatus,
  RemoteRegion,
  RemoteStatus,
  RemoteWarning,
  RigStatus,
} from "@/api";
import type { StatusView } from "@/design";
import { absoluteTime, relativeTime } from "./format";

/** The hint the "stopped connecting" state carries. */
const MISMATCHED_ENV =
  "a .env on the remote that doesn't match this pairing (older setup files, or ones from before you made new setup files)";

/**
 * How long a remote may wait for its first connection before the card suggests
 * what to check. A remote that is running connects within about a minute, so past
 * this a hint helps more than it alarms.
 */
export const STILL_WAITING_AFTER_MS = 5 * 60 * 1000;

/**
 * stillWaitingAt is when a waiting remote's card adds its "Still waiting?" hint
 * (ms since the epoch), timed from the add or the last re-pair; null for a remote
 * that is not waiting.
 */
export function stillWaitingAt(r: Pick<RemoteStatus, "state" | "paired_at">): number | null {
  if (r.state !== "waiting") return null;
  return r.paired_at * 1000 + STILL_WAITING_AFTER_MS;
}

/**
 * remoteStateView is the remote card's state pill plus a help line naming what to
 * check. `now` is injectable for tests.
 */
export function remoteStateView(
  r: Pick<
    RemoteStatus,
    "state" | "direct" | "last_hello_at" | "needs_update" | "ever_connected" | "paired_at"
  >,
  now = Date.now(),
): StatusView {
  switch (r.state) {
    case "connected":
      return r.direct
        ? { tone: "safe", icon: "safe", label: "Connected (direct)" }
        : {
            tone: "safe",
            icon: "safe",
            label: "Connected (relayed, slower)",
            help: "No direct path could be made, so traffic goes through the relay server. Transfers work, more slowly.",
          };
    case "needs_update":
      return r.needs_update === "commander"
        ? {
            tone: "problem",
            icon: "alert",
            label: "Needs update (Commander)",
            help: "This Commander is older than the remote. Update Commander. Pulls continue meanwhile.",
          }
        : {
            tone: "problem",
            icon: "alert",
            label: "Needs update (remote)",
            help: "The remote is older than this Commander. Run `docker compose pull && docker compose up -d` next to its compose file. Pulls continue meanwhile.",
          };
    case "reconnecting":
      return {
        tone: "working",
        icon: "clock",
        label: r.last_hello_at
          ? `Reconnecting (last seen ${relativeTime(r.last_hello_at, now)})`
          : "Reconnecting",
      };
    case "unreachable":
      return {
        tone: "problem",
        icon: "problem",
        label: r.last_hello_at
          ? `Unreachable since ${absoluteTime(r.last_hello_at)}`
          : "Unreachable",
        help: `Possible causes: the remote's container is stopped, its internet link is down, ${MISMATCHED_ENV}, or either side was updated recently and the other hasn't caught up.`,
      };
    case "waiting":
    default: {
      // A re-paired remote that connected before is waiting for its new files,
      // not for its first start.
      const first = r.ever_connected
        ? "Waiting for the remote to start with its new setup files. This changes to Connected about a minute after it does."
        : "Waiting for the remote to start. This changes to Connected about a minute after the remote starts.";
      const late = now >= r.paired_at * 1000 + STILL_WAITING_AFTER_MS;
      return {
        tone: "neutral",
        icon: "clock",
        label: "Waiting for first connection",
        help: late
          ? `${first} Still waiting? Check that the remote computer is online and that its setup files are the latest ones.`
          : first,
      };
    }
  }
}

/** Whether the remote's tunnel is up, so a latency and a path mean something now. */
export function remoteIsUp(r: Pick<RemoteStatus, "state">): boolean {
  return r.state === "connected" || r.state === "needs_update";
}

/**
 * latencyText reads "48 ms, direct" or "210 ms, relayed via Frankfurt" while the
 * tunnel is up, and null otherwise: a down remote's last latency would read as
 * current.
 */
export function latencyText(
  r: Pick<RemoteStatus, "state" | "direct" | "latency_ms" | "relay_region">,
): string | null {
  if (!remoteIsUp(r) || r.latency_ms <= 0) return null;
  const path = r.direct
    ? "direct"
    : r.relay_region
      ? `relayed via ${r.relay_region}`
      : "relayed";
  return `${r.latency_ms} ms, ${path}`;
}

/** rateLimitText is the bandwidth cap as read on the card. */
export function rateLimitText(mbps: number | undefined): string {
  return mbps && mbps > 0 ? `${mbps} MB/s` : "No cap";
}

/**
 * buildAgeNotice is the stale-build warning, naming both versions, or null when
 * the remote's build is recent. It shows whatever the remote's state, since an
 * updater that stopped (or cannot reach the image registry) leaves a remote that
 * otherwise looks healthy.
 */
export function buildAgeNotice(
  r: Pick<RemoteStatus, "build_stale" | "version">,
  commanderVersion: string | undefined,
): string | null {
  if (!r.build_stale) return null;
  return (
    "Remote build is more than ~two weeks older than this Commander — check that its " +
    "updater is running and can reach the image registry. " +
    `Remote ${r.version || "unknown"}, Commander ${commanderVersion || "unknown"}.`
  );
}

/** warningText is one of the remote's recent warnings, in words. */
export function warningText(w: RemoteWarning): string {
  const why = w.reason ? `: ${w.reason}` : "";
  switch (w.kind) {
    case "broadcast_failed":
      return `Couldn't scan the remote's network for ASIAIRs${why}`;
    case "refused_forward":
      return `Refused to forward ${w.proto ? `${w.proto.toUpperCase()} ` : ""}to ${w.target || "an address"}${why}`;
    case "rejected_client":
      return (
        `Rejected a connection from an unknown key${w.client_key ? ` (${w.client_key.slice(0, 16)}…)` : ""}. ` +
        "Its setup files may have leaked; make new setup files to lock it out"
      );
    default:
      return `${w.kind}${why}`;
  }
}

/**
 * relayView is Commander's own reachability of the relay, shown apart from every
 * remote so Commander's internet being down never reads as a remote down. Null
 * when no remote is paired (the snapshot then omits it).
 */
export function relayView(relay: RelayStatus | undefined): StatusView | null {
  if (!relay) return null;
  switch (relay.state) {
    case "up":
      return { tone: "safe", icon: "safe", label: "Commander is online" };
    case "down":
      return {
        tone: "problem",
        icon: "problem",
        label: "Commander can't reach the internet",
        help:
          (relay.since ? `Since ${absoluteTime(relay.since)}. ` : "") +
          "Check this computer's internet connection. Until it's back, every remote shows Reconnecting and then Unreachable, even if the remote itself is fine.",
      };
    case "unknown":
    default:
      return { tone: "neutral", icon: "clock", label: "Checking Commander's connection…" };
  }
}

/**
 * rigsThrough splits the rigs whose last path goes through a remote: `current`
 * are reached through it now, `last` were last reached through it. A rig is
 * reached now when it is online and either its remote's tunnel is up (the hub
 * attaches `remote` only while it is not) or it still holds a live session
 * through it; a paused rig behind a down remote stays online but is not
 * reached.
 */
export function rigsThrough(
  remoteId: string,
  rigs: readonly RigStatus[],
): { current: RigStatus[]; last: RigStatus[] } {
  const mine = rigs.filter((r) => r.site === remoteId);
  const reached = (r: RigStatus) => r.online && (!r.remote || !!r.in_session);
  return {
    current: mine.filter(reached),
    last: mine.filter((r) => !reached(r)),
  };
}

/** devicesSeenThrough is the unadopted devices a remote found on its network. */
export function devicesSeenThrough(
  remoteId: string,
  devices: readonly DiscoveredRig[],
): DiscoveredRig[] {
  return devices.filter((d) => !d.managed && d.site === remoteId);
}

/**
 * regionOptions is the relay-region picker's list. "0" asks the server for its
 * default: on a re-pair (keepRegion set) that keeps the current region, offered
 * first; on Add it is a placeholder until the list loads.
 */
export function regionOptions(
  regions: readonly RemoteRegion[] | null,
  keepRegion: string | undefined,
  failed: boolean,
): { value: string; label: string }[] {
  const keep =
    keepRegion !== undefined
      ? [{ value: "0", label: `Keep the current region${keepRegion ? ` (${keepRegion})` : ""}` }]
      : [];
  if (!regions) {
    if (keep.length) return keep;
    return [{ value: "0", label: failed ? "Nearest (picked by Commander)" : "Measuring the nearest…" }];
  }
  return [
    ...keep,
    ...regions.map((r) => ({
      value: String(r.id),
      label: r.nearest ? `${r.name} (nearest)` : r.name,
    })),
  ];
}

/**
 * initialRegion is what the picker selects once the list loads: the nearest for a
 * new remote, and "0" (keep the current one) for a re-pair.
 */
export function initialRegion(
  regions: readonly RemoteRegion[],
  keepRegion: string | undefined,
): string {
  if (keepRegion !== undefined) return "0";
  const nearest = regions.find((r) => r.nearest);
  return nearest ? String(nearest.id) : "0";
}
