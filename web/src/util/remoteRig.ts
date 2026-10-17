/**
 * How a rig — or a device found on a network — reads when it is reached through a
 * remote: the "remote" pill, the Address fact, whether the Windows `\\ip` shortcut
 * applies, and the card's lines for each state of its remote (design
 * remote-sites §10, second table). Pure, so each
 * remote state's wording is pinned by tests.
 */
import type {
  DiscoveredRig,
  RemoteState,
  RemoteWarning,
  RigStatus,
  SafeToPowerOff,
} from "@/api";
import type { StatusView } from "@/design";
import { absoluteTime, relativeTime } from "./format";

/**
 * isRemote reports whether a rig or device is reached through a remote, from the
 * snapshot's site field: the pill, the Address fact and the shortcut all key off
 * it, since the dashboard card reads only the live snapshot.
 */
export function isRemote(x: { site?: string } | undefined): boolean {
  return !!x?.site;
}

/**
 * addressFact is the Address fact: "ip via name" for a remote, the bare ip on the
 * local network. A remote whose name is not known (yet) still says the address is
 * on another network rather than passing it off as a local one.
 */
export function addressFact(ip: string, site: string | undefined, remoteName: string | undefined): string {
  if (!site) return ip;
  return `${ip} via ${remoteName || "a remote"}`;
}

/**
 * showSmbShortcut says whether the Windows click-to-open `\\ip` shortcut applies:
 * never for a remote's address, which the browser's machine cannot reach.
 */
export function showSmbShortcut(site: string | undefined, isWindows: boolean): boolean {
  return isWindows && !site;
}

/** remoteNameFor looks a site's remote name up, for a found device (which carries no name). */
export function remoteNameFor(
  site: string | undefined,
  remotes: readonly { id: string; name: string }[] | undefined,
): string | undefined {
  if (!site) return undefined;
  return remotes?.find((r) => r.id === site)?.name;
}

/**
 * discoveredHeading names the "Found on your network" section so it reads right
 * when a device in it was seen through a remote rather than on this network.
 */
export function discoveredHeading(devices: readonly Pick<DiscoveredRig, "site">[]): {
  title: string;
  sub: string;
} {
  if (devices.some((d) => !!d.site)) {
    return {
      title: "Found on your networks",
      sub: "Devices that answered a scan on this network or at a remote, but aren't set up yet.",
    };
  }
  return {
    title: "Found on your network",
    sub: "Devices that answered a scan but aren't set up yet.",
  };
}

/** The rig card's lines while its remote's tunnel is not up. */
export interface RigRemoteView {
  /** Replaces the rig's own state pill while the card reads its remote's state. */
  pill: StatusView | null;
  /** "Remote X reconnecting", "Remote X unreachable since T", "Waiting for remote X". */
  line: string | null;
  /** "Last known state as of T: …", the verdict the rig last reported. */
  lastKnown: string | null;
  /** "via X (reconnecting)" on a rig that keeps its own state over a live session. */
  via: string | null;
}

const NONE: RigRemoteView = { pill: null, line: null, lastKnown: null, via: null };

function remoteDownLine(state: RemoteState, name: string, lastHelloAt: number | undefined): string {
  switch (state) {
    case "unreachable":
      return lastHelloAt
        ? `Remote ${name} unreachable since ${absoluteTime(lastHelloAt)}`
        : `Remote ${name} unreachable`;
    case "waiting":
      return `Waiting for remote ${name}`;
    case "reconnecting":
    default:
      return `Remote ${name} reconnecting`;
  }
}

function remoteDownPill(state: RemoteState): StatusView {
  switch (state) {
    case "unreachable":
      return { tone: "problem", icon: "problem", label: "Remote unreachable" };
    case "waiting":
      return { tone: "neutral", icon: "clock", label: "Waiting for remote" };
    case "reconnecting":
    default:
      return { tone: "working", icon: "clock", label: "Remote reconnecting" };
  }
}

function verdictWords(safe: SafeToPowerOff): string {
  switch (safe) {
    case "safe":
      return "safe to power off";
    case "working":
      return "working";
    default:
      return "unknown";
  }
}

/** lastKnownText reads "Last known state as of T: safe to power off, 14 frames not copied". */
export function lastKnownText(lk: NonNullable<RigStatus["last_known"]>): string {
  const pending =
    lk.pending > 0 ? `, ${lk.pending} ${lk.pending === 1 ? "frame" : "frames"} not copied` : "";
  return `Last known state as of ${absoluteTime(lk.at)}: ${verdictWords(lk.safe_to_power_off)}${pending}`;
}

/**
 * rigRemoteView derives a rig card's remote lines. While the worker has written the
 * rig's remote-down state (`remote.down`) the card reads its remote's state, with
 * the rig's last known verdict; a rig that keeps its own state while its remote is
 * down (a live session, or paused) gets the "via X (state)" line instead. A rig
 * whose remote is up, or on the local network, gets nothing extra.
 */
export function rigRemoteView(
  rig: Pick<RigStatus, "remote" | "remote_name" | "last_known" | "site">,
): RigRemoteView {
  const r = rig.remote;
  if (!r) return NONE;
  // The site id still tells remotes apart when the name is not published.
  const name = rig.remote_name || rig.site || "remote";
  if (!r.down) {
    return { ...NONE, via: `via ${name} (${r.state})` };
  }
  return {
    pill: remoteDownPill(r.state),
    line: remoteDownLine(r.state, name, r.last_hello_at),
    lastKnown: rig.last_known ? lastKnownText(rig.last_known) : null,
    via: null,
  };
}

/** refusedForwardText is a refused forward as the rig's card reads it. */
export function refusedForwardText(w: RemoteWarning, now = Date.now()): string {
  const why = w.reason ? `: ${w.reason}` : "";
  return `The remote refused a connection to ${w.target || "this rig"}${why} (${relativeTime(w.at, now)})`;
}
