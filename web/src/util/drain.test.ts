import { describe, expect, it } from "vitest";

import { drainView } from "./drain";

describe("drainView", () => {
  it("reads a count as a count", () => {
    expect(drainView("3 resident")).toEqual({ kind: "count", text: "3 resident" });
  });

  it("reads drained regardless of case", () => {
    expect(drainView("Drained")).toEqual({ kind: "drained" });
  });

  it("treats an unknown summary as not checked, never as a count", () => {
    expect(drainView("unknown")).toEqual({ kind: "unchecked" });
  });
});
