import { describe, expect, it } from "vitest";
import type { RigStatus } from "@/api";
import {
  addressFact,
  discoveredHeading,
  isRemote,
  lastKnownText,
  refusedForwardText,
  remoteNameFor,
  rigRemoteView,
  showSmbShortcut,
} from "./remoteRig";
import { absoluteTime } from "./format";

const NOW = 1_800_000_000_000;
const T = NOW / 1000 - 120;

type RemoteFields = Pick<RigStatus, "remote" | "remote_name" | "last_known">;

describe("the remote pill, Address fact and SMB shortcut", () => {
  it("marks a rig or device by its site field", () => {
    expect(isRemote({ site: "r1" })).toBe(true);
    expect(isRemote({ site: "" })).toBe(false);
    expect(isRemote({})).toBe(false);
    expect(isRemote(undefined)).toBe(false);
  });

  it("reads 'ip via remote name' for a remote rig and the bare ip locally", () => {
    expect(addressFact("192.168.1.40", "r1", "Pier")).toBe("192.168.1.40 via Pier");
    expect(addressFact("192.168.1.40", undefined, undefined)).toBe("192.168.1.40");
    expect(addressFact("192.168.1.40", "", "Pier")).toBe("192.168.1.40");
    expect(addressFact("192.168.1.40", "r1", undefined)).toBe("192.168.1.40 via a remote");
  });

  it("hides the Windows shortcut for a remote's address", () => {
    expect(showSmbShortcut(undefined, true)).toBe(true);
    expect(showSmbShortcut("r1", true)).toBe(false);
    expect(showSmbShortcut(undefined, false)).toBe(false);
  });

  it("finds a found device's remote name by its site", () => {
    const remotes = [{ id: "r1", name: "Pier" }];
    expect(remoteNameFor("r1", remotes)).toBe("Pier");
    expect(remoteNameFor("r2", remotes)).toBeUndefined();
    expect(remoteNameFor("", remotes)).toBeUndefined();
  });
});

describe("discoveredHeading", () => {
  it("keeps the plain heading with no remote-found device", () => {
    expect(discoveredHeading([{ site: "" }, {}]).title).toBe("Found on your network");
  });

  it("reads right once a device was seen through a remote", () => {
    const h = discoveredHeading([{ site: "" }, { site: "r1" }]);
    expect(h.title).toBe("Found on your networks");
    expect(h.sub).toMatch(/at a remote/);
  });
});

describe("rigRemoteView — the rig card for each remote state", () => {
  const lastKnown = { safe_to_power_off: "working" as const, at: T, pending: 14 };

  it("adds nothing while the remote is up or for a local rig", () => {
    const v = rigRemoteView({ remote_name: "Pier" });
    expect(v).toEqual({ pill: null, line: null, lastKnown: null, via: null });
  });

  it("reads 'Remote X reconnecting' with the last known state", () => {
    const rig: RemoteFields = {
      remote_name: "Pier",
      remote: { state: "reconnecting", last_hello_at: T, down: true },
      last_known: lastKnown,
    };
    const v = rigRemoteView(rig);
    expect(v.line).toBe("Remote Pier reconnecting");
    expect(v.pill?.label).toBe("Remote reconnecting");
    expect(v.lastKnown).toBe(
      `Last known state as of ${absoluteTime(T)}: working, 14 frames not copied`,
    );
    expect(v.via).toBeNull();
  });

  it("reads 'Remote X unreachable since T'", () => {
    const v = rigRemoteView({
      remote_name: "Pier",
      remote: { state: "unreachable", last_hello_at: T, down: true },
    });
    expect(v.line).toBe(`Remote Pier unreachable since ${absoluteTime(T)}`);
    expect(v.pill?.label).toBe("Remote unreachable");
    expect(v.lastKnown).toBeNull();
  });

  it("reads 'Waiting for remote X'", () => {
    const v = rigRemoteView({ remote_name: "Pier", remote: { state: "waiting", down: true } });
    expect(v.line).toBe("Waiting for remote Pier");
    expect(v.pill?.label).toBe("Waiting for remote");
  });

  it("names the remote by its site id when its name is not published", () => {
    const v = rigRemoteView({ site: "r2", remote: { state: "waiting", down: true } });
    expect(v.line).toBe("Waiting for remote r2");
    const via = rigRemoteView({ site: "r2", remote: { state: "unreachable", down: false } });
    expect(via.via).toBe("via r2 (unreachable)");
  });

  it("keeps the rig's own state over a live session, with a via line", () => {
    const v = rigRemoteView({
      remote_name: "Pier",
      remote: { state: "reconnecting", last_hello_at: T, down: false },
      last_known: lastKnown,
    });
    expect(v.via).toBe("via Pier (reconnecting)");
    expect(v.pill).toBeNull();
    expect(v.line).toBeNull();
    expect(v.lastKnown).toBeNull();
  });
});

describe("lastKnownText", () => {
  it("names the verdict and leaves out an empty backlog", () => {
    expect(lastKnownText({ safe_to_power_off: "safe", at: T, pending: 0 })).toBe(
      `Last known state as of ${absoluteTime(T)}: safe to power off`,
    );
    expect(lastKnownText({ safe_to_power_off: "unknown", at: T, pending: 1 })).toBe(
      `Last known state as of ${absoluteTime(T)}: unknown, 1 frame not copied`,
    );
  });
});

describe("refusedForwardText", () => {
  it("names the target, the reason and the age", () => {
    expect(
      refusedForwardText(
        { at: T, kind: "refused_forward", target: "10.0.0.9:445", reason: "not an ASIAIR it found" },
        NOW,
      ),
    ).toBe("The remote refused a connection to 10.0.0.9:445: not an ASIAIR it found (2 mins ago)");
  });
});
