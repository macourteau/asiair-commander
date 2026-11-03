import { describe, expect, it } from "vitest";
import { formatExposure } from "./exposure";

describe("formatExposure", () => {
  // The same cases as the Go renderer's TestRenderExposure, so the UI and the
  // folder name agree.
  it.each([
    [0.00100000004749745, "0.001s"],
    [1.67499804496765, "1.675s"],
    [1.32500004768372, "1.325s"],
    [5.63333320617676, "5.633s"],
    [300.0, "300s"],
    [60.0, "60s"],
  ])("renders %f as %s", (input, want) => {
    expect(formatExposure(input)).toBe(want);
  });
});
