// @vitest-environment jsdom
/**
 * The first-run wizard's folder defaults and where "Finish setup" lands. The API
 * and the auth context are faked; the wizard itself, its router and its form run
 * for real.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import type { SetupPayload } from "../auth/AuthContext";

const getSetupStatus = vi.fn();
const completeSetup = vi.fn<(p: SetupPayload) => Promise<void>>();

vi.mock("@/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      scan: () => Promise.resolve([]),
      getSetupStatus: (signal?: AbortSignal) => getSetupStatus(signal),
    },
  };
});

vi.mock("../auth/AuthContext", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../auth/AuthContext")>();
  return { ...actual, useAuth: () => ({ status: "setup", completeSetup }) };
});

const { Setup } = await import("./Setup");

function renderWizard() {
  return render(
    <MemoryRouter initialEntries={["/setup"]}>
      <Routes>
        <Route path="/setup" element={<Setup />} />
        <Route path="/devices" element={<p>Devices page</p>} />
        <Route path="/settings" element={<p>Settings page</p>} />
      </Routes>
    </MemoryRouter>,
  );
}

const lightInput = () => screen.getByLabelText(/^Light frames folder/) as HTMLInputElement;
const stagingInput = () =>
  screen.getByLabelText(/^Temporary folder \(same drive as your pictures\)/) as HTMLInputElement;

beforeEach(() => {
  getSetupStatus.mockReset();
  completeSetup.mockReset();
  completeSetup.mockResolvedValue(undefined);
});
afterEach(cleanup);

describe("Setup — folder defaults", () => {
  it("pre-fills the light and temporary folders on a Windows server, without example paths", async () => {
    getSetupStatus.mockResolvedValue({ needed: true, platform: "other", os: "windows" });
    renderWizard();
    await waitFor(() =>
      expect(lightInput().value).toBe("C:\\Users\\Public\\Pictures\\ASIAIR\\Lights"),
    );
    expect(stagingInput().value).toBe("C:\\Users\\Public\\Pictures\\ASIAIR\\_staging");
    expect(screen.queryByText(/D:\\Astro/)).toBeNull();
  });

  it("leaves the folders empty elsewhere, with examples marked as examples", async () => {
    getSetupStatus.mockResolvedValue({ needed: true, platform: "container", os: "linux" });
    renderWizard();
    await waitFor(() => expect(getSetupStatus).toHaveBeenCalled());
    expect(lightInput().value).toBe("");
    expect(stagingInput().value).toBe("");
    const ex = lightInput().getAttribute("aria-describedby")!.split(" ")
      .map((id) => document.getElementById(id)?.textContent);
    expect(ex).toContain("e.g. D:\\Astro\\Lights");
  });

  it("keeps what the operator typed before the server answered", async () => {
    let answer: (v: unknown) => void = () => {};
    getSetupStatus.mockReturnValue(new Promise((r) => (answer = r)));
    renderWizard();
    fireEvent.change(lightInput(), { target: { value: "E:\\Mine" } });
    answer({ needed: true, platform: "other", os: "windows" });
    await waitFor(() =>
      expect(stagingInput().value).toBe("C:\\Users\\Public\\Pictures\\ASIAIR\\_staging"),
    );
    expect(lightInput().value).toBe("E:\\Mine");
  });
});

describe("Setup — landing", () => {
  it("lands on Devices after Finish setup, with the defaults sent as the roots", async () => {
    getSetupStatus.mockResolvedValue({ needed: true, platform: "other", os: "windows" });
    renderWizard();
    await waitFor(() => expect(lightInput().value).not.toBe(""));

    fireEvent.change(screen.getByLabelText(/^Setup token/), { target: { value: "tok" } });
    fireEvent.change(screen.getByLabelText(/^Set a password/), { target: { value: "hunter22!" } });
    fireEvent.change(screen.getByLabelText(/^Confirm password/), { target: { value: "hunter22!" } });
    fireEvent.click(screen.getByRole("button", { name: "Finish setup" }));

    await screen.findByText("Devices page");
    expect(completeSetup).toHaveBeenCalledTimes(1);
    expect(completeSetup.mock.calls[0]![0].roots).toMatchObject({
      light: "C:\\Users\\Public\\Pictures\\ASIAIR\\Lights",
      staging: "C:\\Users\\Public\\Pictures\\ASIAIR\\_staging",
    });
  });
});
