// @vitest-environment jsdom
/**
 * A loading button must keep keyboard focus: disabling the focused element drops
 * focus to <body>, so the button stays enabled-but-inert (aria-disabled) and
 * swallows activation until the work settles.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Button } from "./Button";

afterEach(cleanup);

describe("Button loading", () => {
  it("keeps focus and ignores clicks while loading", () => {
    const onClick = vi.fn();
    const { rerender } = render(<Button onClick={onClick}>Save</Button>);
    const btn = screen.getByRole("button", { name: "Save" });
    btn.focus();
    rerender(
      <Button onClick={onClick} loading>
        Save
      </Button>,
    );
    expect(document.activeElement).toBe(btn);
    expect(btn).toHaveProperty("disabled", false);
    expect(btn.getAttribute("aria-disabled")).toBe("true");
    expect(btn.getAttribute("aria-busy")).toBe("true");
    fireEvent.click(btn);
    expect(onClick).not.toHaveBeenCalled();
  });

  it("does not submit its form while loading", () => {
    const onSubmit = vi.fn((e: { preventDefault: () => void }) => e.preventDefault());
    render(
      <form onSubmit={onSubmit}>
        <Button type="submit" loading>
          Sign in
        </Button>
      </form>,
    );
    fireEvent.click(screen.getByRole("button", { name: /Sign in/ }));
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("forwards clicks when idle and stays natively disabled when disabled", () => {
    const onClick = vi.fn();
    const { rerender } = render(<Button onClick={onClick}>Go</Button>);
    fireEvent.click(screen.getByRole("button", { name: "Go" }));
    expect(onClick).toHaveBeenCalledTimes(1);
    rerender(
      <Button onClick={onClick} disabled loading>
        Go
      </Button>,
    );
    const btn = screen.getByRole("button", { name: /Go/ });
    expect(btn).toHaveProperty("disabled", true);
    expect(btn.getAttribute("aria-disabled")).toBeNull();
  });
});
