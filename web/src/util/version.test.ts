import { describe, expect, it } from "vitest";
import { newerRelease, updateAvailable } from "./version";

describe("newerRelease", () => {
  it("returns the reported newer release", () => {
    expect(newerRelease({ version: "v1.3.2", latest_version: "v1.4.0" })).toBe("v1.4.0");
  });

  it("returns null when the server reports none", () => {
    expect(newerRelease({ version: "v1.3.2", latest_version: "" })).toBeNull();
    expect(newerRelease({ version: "v1.3.2" })).toBeNull();
  });

  it("returns null before server_info arrives", () => {
    expect(newerRelease(null)).toBeNull();
    expect(newerRelease(undefined)).toBeNull();
  });

  it("returns null for a dev or unset running version", () => {
    expect(newerRelease({ version: "dev", latest_version: "v1.4.0" })).toBeNull();
    expect(newerRelease({ version: "", latest_version: "v1.4.0" })).toBeNull();
  });

  it("returns null when latest is what is installed", () => {
    expect(newerRelease({ version: "v1.4.0", latest_version: "v1.4.0" })).toBeNull();
  });
});

describe("updateAvailable", () => {
  it("reports skew in either direction and ignores dev builds", () => {
    expect(updateAvailable("v1.0.0", "v1.1.0")).toBe("v1.1.0");
    expect(updateAvailable("v1.1.0", "v1.0.0")).toBe("v1.0.0");
    expect(updateAvailable("v1.0.0", "v1.0.0")).toBeNull();
    expect(updateAvailable("dev", "v1.0.0")).toBeNull();
    expect(updateAvailable("v1.0.0", null)).toBeNull();
  });
});
