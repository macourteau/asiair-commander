// @vitest-environment jsdom
/**
 * The toaster's announcement contract: a live region that exists before any
 * toast, so a toast added to it is read; an error is assertive on its own.
 */
import { afterEach, describe, expect, it } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { ToastProvider, useToast } from "./Toast";

afterEach(cleanup);

let api: ReturnType<typeof useToast> | null = null;
function Grab() {
  api = useToast();
  return null;
}

describe("ToastProvider", () => {
  it("announces toasts through a live region mounted before them", () => {
    const { container } = render(
      <ToastProvider>
        <Grab />
      </ToastProvider>,
    );
    const region = container.querySelector(".aac-toaster")!;
    expect(region.getAttribute("aria-live")).toBe("polite");
    expect(region.children).toHaveLength(0);

    act(() => api!.success("Copied 4 files to Bravo"));
    const toast = screen.getByText("Copied 4 files to Bravo");
    expect(region.contains(toast)).toBe(true);
    // No nested status role: the region alone announces it, once.
    expect(toast.closest("[role]")).toBeNull();

    act(() => api!.error("Couldn't reach Commander."));
    expect(screen.getByRole("alert").textContent).toContain("Couldn't reach Commander.");
  });
});
