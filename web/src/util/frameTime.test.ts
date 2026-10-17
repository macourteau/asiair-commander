import { describe, expect, it } from "vitest";
import type { Frame } from "@/api";
import { frameTime } from "./frameTime";

const frame = (f: Partial<Frame>): Frame => ({ mtime: 0, ...f }) as Frame;

describe("frameTime", () => {
  it("prefers the transfer time", () => {
    expect(frameTime(frame({ pulled_at: 1700, mtime: 1500 }))).toBe(1700);
  });

  it("falls back to mtime when the frame was never stamped", () => {
    expect(frameTime(frame({ mtime: 1500 }))).toBe(1500);
    expect(frameTime(frame({ pulled_at: 0, mtime: 1500 }))).toBe(1500);
  });

  it("is 0 for a row with no frame", () => {
    expect(frameTime(undefined)).toBe(0);
    expect(frameTime(frame({}))).toBe(0);
  });
});
