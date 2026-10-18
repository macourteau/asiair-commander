import { describe, expect, it } from "vitest";

import type { DeviceFrames } from "@/api";
import { deviceLine } from "./drain";

const none: DeviceFrames = { in_grace: 0, due: 0, culled: 0, stuck: 0, kept: 0, drained: false };
const frames = (o: Partial<DeviceFrames>): DeviceFrames => ({ ...none, ...o });
const line = (df: DeviceFrames | null, keep = false) => deviceLine(df, keep, "r1", "50P");

describe("deviceLine", () => {
  it("reads a rig not listed since startup as not checked", () => {
    expect(line(null).kind).toBe("unchecked");
  });

  it("reads a drained device as drained", () => {
    expect(line(frames({ drained: true })).kind).toBe("drained");
  });

  it("hides the line when nothing but uncopied frames is on the device", () => {
    expect(line(frames({})).kind).toBe("hidden");
  });

  it("says frames all in grace as a sentence, with the badge", () => {
    const l = line(frames({ in_grace: 3 }));
    expect(l).toMatchObject({ kind: "frames", text: "3 frames on the device are in grace", badge: "in_grace", links: [] });
    expect(line(frames({ in_grace: 1 }))).toMatchObject({ text: "1 frame on the device is in grace" });
  });

  it("names each reason when frames stay for more than one", () => {
    const l = line(frames({ in_grace: 3, stuck: 2 }));
    expect(l).toMatchObject({
      kind: "frames",
      text: "5 still on the device: 3 in grace, 2 can't be removed automatically",
      badge: "in_grace",
    });
  });

  it("never claims stuck frames are in grace", () => {
    const l = line(frames({ stuck: 2 }));
    expect(l).toMatchObject({ kind: "frames", text: "2 frames on the device can't be removed automatically", badge: null });
    expect(line(frames({ stuck: 1 }))).toMatchObject({ text: "1 frame on the device can't be removed automatically" });
  });

  it("gives the stuck reason the problem tone and links to why they stay", () => {
    const l = line(frames({ in_grace: 3, stuck: 2 }));
    if (l.kind !== "frames") throw new Error(l.kind);
    expect(l.parts.filter((p) => p.problem).map((p) => p.text)).toEqual(["2 can't be removed automatically"]);
    expect(l.links).toEqual([
      {
        to: "/history?rig=r1&view=never_deletable",
        text: "Why they stay",
        label: "Why they stay: 50P's frames that can't be removed, in History",
      },
    ]);
  });

  it("says frames past grace are due for removal", () => {
    expect(line(frames({ due: 4 }))).toMatchObject({ text: "4 frames on the device are due for removal", badge: null });
    expect(line(frames({ due: 1 }))).toMatchObject({ text: "1 frame on the device is due for removal" });
  });

  it("says culled frames are leaving, with a link to the missing-frames panel", () => {
    const l = line(frames({ culled: 1 }));
    expect(l).toMatchObject({ text: "1 frame removed from the library is leaving the device", badge: null });
    expect(line(frames({ culled: 2 }))).toMatchObject({ text: "2 frames removed from the library are leaving the device" });
    if (l.kind !== "frames") throw new Error(l.kind);
    expect(l.links).toEqual([
      {
        to: "/devices?rig=r1&show=missing",
        text: "Missing frames",
        label: "Missing frames: 50P's frames removed from the library, in Devices",
      },
    ]);
  });

  it("lists every reason in a fixed order", () => {
    expect(line(frames({ in_grace: 1, due: 2, culled: 3, stuck: 4 }))).toMatchObject({
      text: "10 still on the device: 1 in grace, 2 due for removal, 3 removed from the library, 4 can't be removed automatically",
    });
  });

  it("reads a keep-originals rig as kept, naming its stuck frames after", () => {
    expect(line(frames({ kept: 12 }), true)).toMatchObject({
      text: "12 frames are kept on the device",
      badge: "keep_originals",
      links: [],
    });
    const l = line(frames({ kept: 12, stuck: 2 }), true);
    expect(l).toMatchObject({
      text: "12 frames are kept on the device; 2 can't be removed automatically",
      badge: "keep_originals",
    });
    if (l.kind !== "frames") throw new Error(l.kind);
    expect(l.links.map((k) => k.text)).toEqual(["Why they stay"]);
  });

  it("shows a keep-originals rig's line even with nothing kept", () => {
    expect(line(frames({}), true)).toMatchObject({ text: "0 frames are kept on the device", badge: "keep_originals" });
    expect(line(frames({ kept: 1 }), true)).toMatchObject({ text: "1 frame is kept on the device" });
  });
});
