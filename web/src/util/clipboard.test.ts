// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { base64Bytes, downloadBlob } from "./clipboard";

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("downloadBlob", () => {
  it("saves under the given name and revokes the blob URL only after the browser has had time to read it", () => {
    vi.useFakeTimers();
    const blob = new Blob(["PK"], { type: "application/zip" });
    const create = vi.fn((_: Blob) => "blob:x");
    const revoke = vi.fn();
    Object.assign(URL, { createObjectURL: create, revokeObjectURL: revoke });
    let saved = "";
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      saved = this.download;
    });

    downloadBlob("asiair-remote.zip", blob);

    expect(create).toHaveBeenCalledWith(blob);
    expect(click).toHaveBeenCalledOnce();
    expect(saved).toBe("asiair-remote.zip");
    expect(revoke).not.toHaveBeenCalled();
    vi.runAllTimers();
    expect(revoke).toHaveBeenCalledWith("blob:x");
  });
});

describe("base64Bytes", () => {
  it("decodes binary bytes exactly, including ones above 0x7f", () => {
    // "UEsDBP8A" is the bytes 50 4b 03 04 ff 00: a zip signature, then 0xff and 0x00.
    expect(Array.from(base64Bytes("UEsDBP8A"))).toEqual([0x50, 0x4b, 0x03, 0x04, 0xff, 0x00]);
  });
});
