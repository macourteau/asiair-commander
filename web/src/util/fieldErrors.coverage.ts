/**
 * Coverage guard for the shared {@link fieldErrors} mapper (C3, web half).
 *
 * The checked-in fixture `__fixtures__/validator_fields.txt` is a verbatim copy of
 * the Go `internal/config/testdata/validator_fields.txt` — every Field the backend
 * validator can emit, with `rigs[<id>]` normalized to `rigs[*]`. This module proves
 * the mapper keys every one of those Fields to a real input (never to the hosts
 * sentinel, and never to nothing), so no backend field can silently regress to the
 * banner on the frontend either.
 *
 * It is deliberately framework-free: the web project has no test runner yet, so the
 * assertion is exposed as a pure function a runner (or a manual check) can call —
 * `expect(uncoveredValidatorFields()).toEqual([])`. tsc compiles it as part of the
 * project, so drift that breaks the types is still caught by `tsc --noEmit`.
 */
import fixtureRaw from "./__fixtures__/validator_fields.txt?raw";
import { fieldErrors, HOSTS_KEY } from "./fieldErrors";
import type { Problem } from "@/api";

/** The fixture's Field lines, `rigs[*]`-normalized, blank lines dropped. */
export const validatorFieldFixture: string[] = fixtureRaw
  .split("\n")
  .map((l) => l.trim())
  .filter((l) => l.length > 0);

/**
 * Returns any fixture Field the mapper fails to key to a real input — either it
 * produced no key, or it fell to the hosts sentinel. An empty result means full
 * coverage. `rigId` stands in for the concrete id a real `rigs[<id>].X` Problem
 * carries.
 */
export function uncoveredValidatorFields(rigId = "rig-x"): string[] {
  const uncovered: string[] = [];
  for (const field of validatorFieldFixture) {
    const concrete = field.replace("rigs[*]", `rigs[${rigId}]`);
    const problems: Problem[] = [{ Severity: "error", Field: concrete, Message: "x" }];
    const keys = Object.keys(fieldErrors(problems, { rigId }));
    if (keys.length === 0 || keys.includes(HOSTS_KEY)) {
      uncovered.push(field);
    }
  }
  return uncovered;
}
