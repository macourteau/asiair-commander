/**
 * Runs the coverage guard in fieldErrors.coverage.ts: every Field the Go
 * validator can emit must key to a real input, so no backend validation error
 * silently falls back to the page-level banner.
 */
import { expect, it } from "vitest";
import { uncoveredValidatorFields, validatorFieldFixture } from "./fieldErrors.coverage";

it("keys every validator field to an input", () => {
  expect(validatorFieldFixture.length).toBeGreaterThan(0); // fixture actually loaded
  expect(uncoveredValidatorFields()).toEqual([]);
});
