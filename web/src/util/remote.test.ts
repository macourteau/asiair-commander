import { describe, expect, it } from "vitest";
import type { DiscoveredRig, RemoteStatus, RigStatus } from "@/api";
import {
  buildAgeNotice,
  devicesSeenThrough,
  initialRegion,
  regionOptions,
  latencyText,
  rateLimitText,
  relayView,
  remoteStateView,
  rigsThrough,
  stillWaitingAt,
  STILL_WAITING_AFTER_MS,
  warningText,
} from "./remote";
import { absoluteTime } from "./format";

const NOW = 1_800_000_000_000;
const T = NOW / 1000 - 300; // five minutes before NOW

function remote(over: Partial<RemoteStatus>): RemoteStatus {
  return {
    id: "r1",
    name: "Pier",
    state: "connected",
    direct: true,
    region: "fra",
    latency_ms: 48,
    uptime_s: 0,
    restarts: 0,
    fingerprint: "ab12",
    ever_connected: true,
    created_at: 0,
    paired_at: 0,
    ...over,
  };
}

describe("remoteStateView", () => {
  it("reads each state as the card table of design remote-sites §10 gives it", () => {
    expect(remoteStateView(remote({ state: "waiting" }), NOW).label).toBe(
      "Waiting for first connection",
    );
    expect(remoteStateView(remote({ direct: true }), NOW).label).toBe("Connected (direct)");
    expect(remoteStateView(remote({ direct: false }), NOW).label).toBe(
      "Connected (relayed, slower)",
    );
    expect(
      remoteStateView(remote({ state: "reconnecting", last_hello_at: T }), NOW).label,
    ).toBe("Reconnecting (last seen 5 mins ago)");
    expect(
      remoteStateView(remote({ state: "unreachable", last_hello_at: T }), NOW).label,
    ).toBe(`Unreachable since ${absoluteTime(T)}`);
  });

  it("names the older side when a digest algorithm differs", () => {
    expect(
      remoteStateView(remote({ state: "needs_update", needs_update: "remote" }), NOW).label,
    ).toBe("Needs update (remote)");
    expect(
      remoteStateView(remote({ state: "needs_update", needs_update: "commander" }), NOW)
        .label,
    ).toBe("Needs update (Commander)");
  });

  it("says a waiting remote connects about a minute after it starts, with no hint yet", () => {
    const fresh = remoteStateView(
      remote({ state: "waiting", ever_connected: false, paired_at: NOW / 1000 - 60 }),
      NOW,
    ).help!;
    expect(fresh).toMatch(/^Waiting for the remote to start\. .*about a minute/);
    expect(fresh).not.toMatch(/Still waiting/);
    expect(fresh).not.toMatch(/\.env/);
  });

  it("adds what to check once a remote has waited five minutes since it was paired", () => {
    const paired = NOW / 1000 - STILL_WAITING_AFTER_MS / 1000;
    const late = remoteStateView(
      remote({ state: "waiting", ever_connected: false, paired_at: paired }),
      NOW,
    ).help!;
    expect(late).toMatch(/Still waiting\? Check that the remote computer is online/);
    expect(late).toMatch(/setup files are the latest ones/);
    // One second earlier it has not waited long enough.
    expect(
      remoteStateView(remote({ state: "waiting", ever_connected: false, paired_at: paired }), NOW - 1000)
        .help,
    ).not.toMatch(/Still waiting/);
  });

  it("words a re-paired remote's wait around its new setup files", () => {
    const help = remoteStateView(
      remote({ state: "waiting", ever_connected: true, paired_at: NOW / 1000 }),
      NOW,
    ).help!;
    expect(help).toMatch(/new setup files/);
    expect(help).not.toMatch(/Still waiting/);
  });

  it("times the hint from the pairing, and only while waiting", () => {
    expect(stillWaitingAt(remote({ state: "waiting", paired_at: 100 }))).toBe(
      100_000 + STILL_WAITING_AFTER_MS,
    );
    expect(stillWaitingAt(remote({ state: "connected", paired_at: 100 }))).toBeNull();
  });

  it("hints at a mismatched .env when unreachable", () => {
    const unreachable = remoteStateView(remote({ state: "unreachable", last_hello_at: T }), NOW);
    expect(unreachable.help).toMatch(/stopped/);
    expect(unreachable.help).toMatch(/\.env/);
    expect(unreachable.help).toMatch(/updated recently/);
  });

  it("drops the time when there was never a hello", () => {
    expect(remoteStateView(remote({ state: "reconnecting" }), NOW).label).toBe("Reconnecting");
    expect(remoteStateView(remote({ state: "unreachable" }), NOW).label).toBe("Unreachable");
  });
});

describe("latencyText", () => {
  it("gives the latency and path while the tunnel is up", () => {
    expect(latencyText(remote({ direct: true }))).toBe("48 ms, direct");
    expect(latencyText(remote({ direct: false, relay_region: "Frankfurt" }))).toBe(
      "48 ms, relayed via Frankfurt",
    );
    expect(latencyText(remote({ direct: false }))).toBe("48 ms, relayed");
    expect(latencyText(remote({ state: "needs_update" }))).toBe("48 ms, direct");
  });

  it("says nothing while the tunnel is down or before any hello", () => {
    expect(latencyText(remote({ state: "reconnecting" }))).toBeNull();
    expect(latencyText(remote({ state: "unreachable" }))).toBeNull();
    expect(latencyText(remote({ state: "waiting" }))).toBeNull();
    expect(latencyText(remote({ latency_ms: 0 }))).toBeNull();
  });
});

describe("rateLimitText", () => {
  it("reads the cap, or no cap when off", () => {
    expect(rateLimitText(2.5)).toBe("2.5 MB/s");
    expect(rateLimitText(0)).toBe("No cap");
    expect(rateLimitText(undefined)).toBe("No cap");
  });
});

describe("buildAgeNotice", () => {
  it("names both versions once the remote's build is stale", () => {
    const n = buildAgeNotice(remote({ build_stale: true, version: "v1.30.0" }), "v1.32.1");
    expect(n).toMatch(/more than ~two weeks older than this Commander/);
    expect(n).toMatch(/updater is running and can reach the image registry/);
    expect(n).toMatch(/Remote v1\.30\.0, Commander v1\.32\.1/);
  });

  it("shows whatever the remote's state", () => {
    expect(buildAgeNotice(remote({ state: "unreachable", build_stale: true }), "v1")).not.toBeNull();
  });

  it("is absent for a recent build", () => {
    expect(buildAgeNotice(remote({ version: "v1.30.0" }), "v1.32.1")).toBeNull();
  });
});

describe("warningText", () => {
  it("words each warning kind", () => {
    expect(warningText({ at: 0, kind: "broadcast_failed", reason: "no route" })).toBe(
      "Couldn't scan the remote's network for ASIAIRs: no route",
    );
    expect(
      warningText({
        at: 0,
        kind: "refused_forward",
        proto: "tcp",
        target: "10.0.0.9:22",
        reason: "port not allowed",
      }),
    ).toBe("Refused to forward TCP to 10.0.0.9:22: port not allowed");
    expect(
      warningText({ at: 0, kind: "rejected_client", client_key: "nodekey:0123456789abcdef0123" }),
    ).toMatch(/^Rejected a connection from an unknown key \(nodekey:01234567…\)\. .*make new setup files/);
  });
});

describe("relayView", () => {
  it("is absent when no remote is paired", () => {
    expect(relayView(undefined)).toBeNull();
  });

  it("reads Commander's own reachability apart from any remote", () => {
    expect(relayView({ state: "up", since: T })!.label).toBe("Commander is online");
    const down = relayView({ state: "down", since: T })!;
    expect(down.label).toBe("Commander can't reach the internet");
    expect(down.help!.startsWith(`Since ${absoluteTime(T)}. `)).toBe(true);
    expect(down.help).toMatch(/even if the remote itself is fine/);
    expect(relayView({ state: "down", since: 0 })!.help).toMatch(/^Check this computer/);
    expect(relayView({ state: "unknown", since: 0 })!.tone).toBe("neutral");
  });
});

describe("rigsThrough and devicesSeenThrough", () => {
  const rig = (id: string, site: string | undefined, online: boolean) =>
    ({ id, site, online }) as RigStatus;

  it("splits the rigs reached through a remote from those last reached through it", () => {
    const out = rigsThrough("r1", [
      rig("a", "r1", true),
      rig("b", "r1", false),
      rig("c", undefined, true),
      rig("d", "r2", true),
    ]);
    expect(out.current.map((r) => r.id)).toEqual(["a"]);
    expect(out.last.map((r) => r.id)).toEqual(["b"]);
  });

  it("counts a rig behind a down remote as reached only while it holds a live session", () => {
    const down = { state: "unreachable", down: false } as RigStatus["remote"];
    const paused = { ...rig("paused", "r1", true), paused: true, remote: down } as RigStatus;
    const live = { ...rig("live", "r1", true), in_session: true, remote: down } as RigStatus;
    const out = rigsThrough("r1", [paused, live]);
    expect(out.current.map((r) => r.id)).toEqual(["live"]);
    expect(out.last.map((r) => r.id)).toEqual(["paused"]);
  });

  it("lists only the unadopted devices seen through that remote", () => {
    const d = (guid: string, site: string, managed: boolean) =>
      ({ guid, site, managed }) as DiscoveredRig;
    expect(
      devicesSeenThrough("r1", [d("x", "r1", false), d("y", "r1", true), d("z", "", false)]).map(
        (x) => x.guid,
      ),
    ).toEqual(["x"]);
  });
});

describe("regionOptions and initialRegion", () => {
  const regions = [
    { id: 1, code: "nyc", name: "New York City", nearest: true },
    { id: 4, code: "fra", name: "Frankfurt" },
  ];

  it("preselects the nearest region for a new remote", () => {
    expect(regionOptions(regions, undefined, false)).toEqual([
      { value: "1", label: "New York City (nearest)" },
      { value: "4", label: "Frankfurt" },
    ]);
    expect(initialRegion(regions, undefined)).toBe("1");
    expect(initialRegion([{ id: 4, code: "fra", name: "Frankfurt" }], undefined)).toBe("0");
  });

  it("offers and keeps the current region first on a re-pair", () => {
    const opts = regionOptions(regions, "fra", false);
    expect(opts[0]).toEqual({ value: "0", label: "Keep the current region (fra)" });
    expect(opts).toHaveLength(3);
    expect(initialRegion(regions, "fra")).toBe("0");
  });

  it("reads a placeholder until the list loads, or after it fails", () => {
    expect(regionOptions(null, undefined, false)).toEqual([
      { value: "0", label: "Measuring the nearest…" },
    ]);
    expect(regionOptions(null, undefined, true)[0]!.label).toBe("Nearest (picked by Commander)");
    expect(regionOptions(null, "fra", true)).toEqual([
      { value: "0", label: "Keep the current region (fra)" },
    ]);
  });
});
