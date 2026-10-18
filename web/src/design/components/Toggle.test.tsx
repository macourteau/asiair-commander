// @vitest-environment jsdom
/**
 * A switch inside a <Field> is named by the field label, not by its visible
 * "On"/"Off" state text, and carries the field's help as its description.
 */
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { Field } from "./Field";
import { Toggle } from "./Toggle";

afterEach(cleanup);

describe("Toggle in a Field", () => {
  it("takes its name from the field label and its description from the help", () => {
    render(
      <Field label="Strip spaces" help="Remove spaces from folder names.">
        {(f) => <Toggle {...f} stateLabel checked onChange={() => {}} label="On" />}
      </Field>,
    );
    const sw = screen.getByRole("switch", { name: "Strip spaces" });
    expect(sw).toHaveProperty("checked", true);
    expect(screen.getByRole("switch", { description: "Remove spaces from folder names." })).toBe(sw);
  });

  it("is named by its own label when standalone", () => {
    render(<Toggle checked={false} onChange={() => {}} label="Show acknowledged" />);
    expect(screen.getByRole("switch", { name: "Show acknowledged" })).toBeTruthy();
  });
});
