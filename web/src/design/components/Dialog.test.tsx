// @vitest-environment jsdom
/**
 * Dialog behaviour under a re-rendering parent. Every consumer passes an inline
 * `onClose={() => setSelected(null)}`, so the callback's identity changes on each
 * parent render — and surfaces like History re-render whenever a live frame event
 * lands. The dialog's focus/scroll-lock effect must not be torn down and re-run by
 * that churn: re-running it re-focuses the header, and since the dialog element is
 * itself the scroll container, that yanks a reader back to the top mid-scroll.
 */
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { useRef } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Dialog } from "./Dialog";

// jsdom does no layout, so offsetParent is null for every element and the focus
// trap's visibility filter would discard the whole dialog. Stand in for layout:
// an attached element is visible, matching what the browser reports.
beforeAll(() => {
  Object.defineProperty(HTMLElement.prototype, "offsetParent", {
    configurable: true,
    get(this: HTMLElement) {
      return this.parentElement;
    },
  });
});

afterEach(cleanup);

/** A consumer shaped like the real surfaces: fresh inline onClose every render. */
function Harness({ tick, onClose }: { tick: number; onClose: () => void }) {
  return (
    <Dialog open onClose={() => onClose()} title="Light_0011.fit">
      <button type="button">Detail {tick}</button>
    </Dialog>
  );
}

describe("Dialog", () => {
  it("moves focus into the dialog on open", () => {
    render(<Harness tick={0} onClose={() => {}} />);
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Close" }));
  });

  it("leaves focus alone when the parent re-renders", () => {
    const { rerender } = render(<Harness tick={0} onClose={() => {}} />);

    // The reader tabs (or scrolls) into the body.
    const detail = screen.getByRole("button", { name: "Detail 0" });
    detail.focus();
    expect(document.activeElement).toBe(detail);

    // A live event refetches: same open dialog, new body content, new onClose identity.
    rerender(<Harness tick={1} onClose={() => {}} />);

    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Detail 1" }));
  });

  it("closes on Escape with the latest onClose", () => {
    const stale = vi.fn();
    const fresh = vi.fn();
    const { rerender } = render(<Harness tick={0} onClose={stale} />);
    rerender(<Harness tick={1} onClose={fresh} />);

    fireEvent.keyDown(document, { key: "Escape" });

    expect(fresh).toHaveBeenCalledTimes(1);
    expect(stale).not.toHaveBeenCalled();
  });

  it("restores focus to the opener once, on close", () => {
    const opener = document.createElement("button");
    document.body.appendChild(opener);
    opener.focus();

    const { rerender } = render(<Harness tick={0} onClose={() => {}} />);
    rerender(<Harness tick={1} onClose={() => {}} />);
    expect(document.activeElement).not.toBe(opener); // not bounced out mid-life

    cleanup();
    expect(document.activeElement).toBe(opener);
    opener.remove();
  });

  it("focuses the initial-focus element on open and still restores the opener", () => {
    const opener = document.createElement("button");
    document.body.appendChild(opener);
    opener.focus();

    function Field() {
      const ref = useRef<HTMLInputElement>(null);
      return (
        <Dialog open onClose={() => {}} title="Rename ref.fit" initialFocus={ref}>
          <button type="button">Before</button>
          <input ref={ref} aria-label="New name" defaultValue="ref.fit" />
        </Dialog>
      );
    }
    render(<Field />);
    expect(document.activeElement).toBe(screen.getByRole("textbox", { name: "New name" }));

    cleanup();
    expect(document.activeElement).toBe(opener);
    opener.remove();
  });
});
