// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { Field } from "./Field";

afterEach(cleanup);

describe("Field", () => {
  it("marks an example as one, so it never reads as the field's value", () => {
    render(
      <Field label="Grace" help="How long to wait." example="1h">
        {(f) => <input {...f} defaultValue="15s" />}
      </Field>,
    );
    const input = screen.getByLabelText("Grace");
    expect(input.getAttribute("aria-describedby")).toBeTruthy();
    const ids = input.getAttribute("aria-describedby")!.split(" ");
    const texts = ids.map((id) => document.getElementById(id)?.textContent);
    expect(texts).toEqual(["How long to wait.", "e.g. 1h"]);
  });

  it("renders a note (a live readout) without the example prefix", () => {
    render(
      <Field label="Reserve" note="5 GiB = 5.4 GB">
        {(f) => <input {...f} />}
      </Field>,
    );
    const input = screen.getByLabelText("Reserve");
    const id = input.getAttribute("aria-describedby")!;
    expect(document.getElementById(id)?.textContent).toBe("5 GiB = 5.4 GB");
  });
});
