// @vitest-environment jsdom
/**
 * RigCard is exported (over private-by-default) so this rendering test can drive
 * it directly — no SSE snapshot context to fake, since it takes `rig` as a plain
 * prop rather than reading it live.
 */
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { RigCard } from "./Dashboard";
import type { RigStatus } from "@/api";

afterEach(cleanup);

function baseRig(overrides: Partial<RigStatus>): RigStatus {
  return {
    id: "r1",
    name: "50P",
    state: "idle",
    online: true,
    paused: false,
    requested: "running",
    frames_tonight: 0,
    shares: [],
    device_frames: null,
    keep_originals: false,
    safe_to_power_off: "safe",
    listing_ts: 0,
    ...overrides,
  };
}

describe("RigCard — monitoring ring", () => {
  it("shows the countdown ring on a clear, idle rig", () => {
    render(<RigCard rig={baseRig({ next_poll_at: 0 })} alerts={[]} />);
    expect(screen.getByLabelText(/Monitoring/)).toBeTruthy();
  });

  it("hides the ring in favor of the plain pill while a run is in progress", () => {
    render(
      <RigCard
        rig={baseRig({ requested: "run_until_idle", run_until_idle: true })}
        alerts={[]}
      />,
    );
    expect(screen.queryByLabelText(/^Monitoring/)).toBeNull();
    expect(screen.getByText("Running until idle")).toBeTruthy();
  });

  it("hides the ring while a delete backlog is draining", () => {
    render(<RigCard rig={baseRig({ draining: true })} alerts={[]} />);
    expect(screen.queryByLabelText(/^Monitoring/)).toBeNull();
    expect(screen.getByText("Monitoring")).toBeTruthy();
  });
});

describe("RigCard — Safe box as-of line", () => {
  it("dates a paused rig's kept verdict", () => {
    render(
      <RigCard
        rig={baseRig({
          state: "paused",
          paused: true,
          requested: "paused",
          safe_to_power_off: "safe",
          safe_as_of: Math.floor(Date.now() / 1000) - 60,
        })}
        alerts={[]}
      />,
    );
    expect(screen.getByText(/as of/)).toBeTruthy();
  });

  it("never dates the Unknown a plain pause produced", () => {
    render(
      <RigCard
        rig={baseRig({
          state: "paused",
          paused: true,
          requested: "paused",
          safe_to_power_off: "unknown",
          power_off_reason: "paused",
          safe_as_of: Math.floor(Date.now() / 1000) - 60,
        })}
        alerts={[]}
      />,
    );
    expect(screen.queryByText(/as of/)).toBeNull();
  });

  it("never shows on an unpaused rig even with a safe_as_of", () => {
    render(
      <RigCard
        rig={baseRig({ safe_as_of: Math.floor(Date.now() / 1000) - 60 })}
        alerts={[]}
      />,
    );
    expect(screen.queryByText(/as of/)).toBeNull();
  });
});

describe("RigCard — reached through a remote", () => {
  it("shows the remote pill for a rig on a remote's site", () => {
    render(<RigCard rig={baseRig({ site: "r1", remote_name: "Pier" })} alerts={[]} />);
    expect(screen.getByText("remote")).toBeTruthy();
  });

  it("shows no remote pill for a local rig", () => {
    render(<RigCard rig={baseRig({})} alerts={[]} />);
    expect(screen.queryByText("remote")).toBeNull();
  });

  it("reads the remote's state, not Offline, while its tunnel is down", () => {
    render(
      <RigCard
        rig={baseRig({
          state: "offline",
          online: false,
          safe_to_power_off: "unknown",
          power_off_reason: "remote",
          site: "r1",
          remote_name: "Pier",
          remote: { state: "reconnecting", last_hello_at: 1_700_000_000, down: true },
          last_known: { safe_to_power_off: "working", at: 1_700_000_000, pending: 14 },
        })}
        alerts={[]}
      />,
    );
    expect(screen.queryByText("Offline")).toBeNull();
    expect(screen.getByText("Remote reconnecting")).toBeTruthy();
    expect(screen.getByText("Remote Pier reconnecting")).toBeTruthy();
    expect(screen.getByText(/14 frames not copied/)).toBeTruthy();
    expect(screen.getByText("Unknown — don't rely on this")).toBeTruthy();
  });

  it("keeps a rig's own state over a live session, with the via line", () => {
    render(
      <RigCard
        rig={baseRig({
          state: "pulling",
          site: "r1",
          remote_name: "Pier",
          in_session: true,
          remote: { state: "reconnecting", down: false },
        })}
        alerts={[]}
      />,
    );
    expect(screen.getByText("Transferring")).toBeTruthy();
    expect(screen.getByText("via Pier (reconnecting)")).toBeTruthy();
  });
});

describe("RigCard — device line", () => {
  const frames = { in_grace: 0, due: 0, culled: 0, stuck: 0, kept: 0, drained: false };
  const card = (rig: Partial<RigStatus>) =>
    render(
      <MemoryRouter>
        <RigCard rig={baseRig(rig)} alerts={[]} />
      </MemoryRouter>,
    );

  it("says a rig never listed is not checked yet", () => {
    card({ device_frames: null });
    expect(screen.getByText("Device not checked yet")).toBeTruthy();
    expect(screen.queryByText("in grace")).toBeNull();
  });

  it("separates stuck frames from those in grace and links to why they stay", () => {
    card({ device_frames: { ...frames, in_grace: 3, stuck: 2 } });
    expect(screen.getByText("2 can't be removed automatically")).toBeTruthy();
    expect(screen.getByText("in grace")).toBeTruthy();
    const link = screen.getByRole("link", { name: /^Why they stay/ });
    expect(link.getAttribute("href")).toBe("/history?rig=r1&view=never_deletable");
    expect(link.getAttribute("aria-label")).toContain("50P");
  });

  it("shows no in-grace badge when nothing is in grace", () => {
    card({ device_frames: { ...frames, stuck: 2 } });
    expect(screen.getByText("can't be removed automatically")).toBeTruthy();
    expect(screen.queryByText("in grace")).toBeNull();
  });

  it("links culled frames to the rig's missing-frames panel", () => {
    card({ device_frames: { ...frames, culled: 1 } });
    const link = screen.getByRole("link", { name: /^Missing frames/ });
    expect(link.getAttribute("href")).toBe("/devices?rig=r1&show=missing");
  });

  it("hides the line while only uncopied frames sit on the device", () => {
    card({ device_frames: { ...frames } });
    expect(screen.queryByText(/on the device/)).toBeNull();
    expect(screen.queryByText(/Device/)).toBeNull();
  });
});
