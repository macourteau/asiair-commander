// @vitest-environment jsdom
/**
 * Settings' unsaved-edits guard: leaving the page through in-app navigation with
 * a pending edit asks before discarding it, and a clean page leaves silently.
 */
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createMemoryRouter, Link, RouterProvider } from "react-router";
import { ToastProvider } from "@/design";
import { Settings } from "./Settings";

// Settings reads the solver status from the live snapshot; no SSE stream here.
vi.mock("@/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api")>()),
  useSnapshot: () => null,
}));

// jsdom does no layout, so offsetParent is null everywhere and the Dialog's focus
// trap would see nothing focusable. An attached element counts as visible.
beforeAll(() => {
  Object.defineProperty(HTMLElement.prototype, "offsetParent", {
    configurable: true,
    get(this: HTMLElement) {
      return this.parentElement;
    },
  });
});

const CONFIG = {
  schema_version: 4,
  web_port: 8686,
  strip_target_spaces: true,
  mosaic_detection: false,
  update_check_disabled: false,
  roots: {
    light: "/archive/light",
    cal: "/archive/cal",
    staging: "/archive/.staging",
    logs: "/archive/logs",
  },
  exclude_extensions: ["jpg"],
  delete_sidecar_extensions: ["jpg"],
  disk_reserve_bytes: 5368709120,
  max_concurrent_transfers: 3,
  previews_enabled: true,
  preview_concurrency: 2,
  preview_cache_max_bytes: 2147483648,
  plate_solve_enabled: false,
  plate_solve_radius_deg: 10,
  missing_display_period: "0s",
  defaults: {
    poll_interval: "45s",
    retry_interval: "5m0s",
    settle_cycles: 1,
    min_file_age: "0s",
    grace: "1h0m0s",
    failed_retry: { base: "2m0s", factor: 2, max: "1h0m0s", max_attempts: 6 },
    unreachable_alert_after: "15m0s",
    reconcile_settle: "24h0m0s",
  },
  web: {
    bcrypt_cost: 12,
    session_idle_timeout: "168h0m0s",
    session_max_age: "720h0m0s",
    login_backoff_base: "1s",
    login_backoff_cap: "30s",
    login_global_cap: 20,
    allowed_hosts: ["localhost"],
    metrics_expose_lan: false,
  },
  smb: { remove_retry_budget: 10 },
  date_format: "2006-01-02",
  alias_table: { light: "LIGHT" },
  sensor_map: {},
  templates: {
    light: "<TARGET>/<OTA>/<SESSION>/NIGHT_<n>",
    cal: { BIAS: "<SENSOR>/BIAS", DARK: "<SENSOR>/DARK", FLAT: "<SENSOR>/FLAT" },
    mosaic: "<TARGET_BASE>/<OTA>/<SESSION>/<TARGET_BASE>_<PANEL>/NIGHT_<n>",
  },
  rigs: [],
  updated_at: "1",
};

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input).split("?")[0];
      if (path === "/api/config") return json(200, CONFIG);
      // Panels that load their own data (library links, previews) are not under
      // test; an empty error keeps them quiet.
      return json(404, { error: "not found" });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function renderAt() {
  const router = createMemoryRouter(
    [
      {
        path: "/settings",
        element: (
          <>
            <Link to="/">Dashboard</Link>
            <Settings />
          </>
        ),
      },
      { path: "/", element: <h1>Dashboard page</h1> },
    ],
    { initialEntries: ["/settings"] },
  );
  render(
    <ToastProvider>
      <RouterProvider router={router} />
    </ToastProvider>,
  );
  return router;
}

async function editPollInterval() {
  const input = (await screen.findByLabelText("Poll interval")) as HTMLInputElement;
  fireEvent.change(input, { target: { value: "90s" } });
  expect(input.value).toBe("90s");
}

describe("Settings unsaved-edits guard", () => {
  it("leaves without asking when nothing changed", async () => {
    const router = renderAt();
    await screen.findByLabelText("Poll interval");
    fireEvent.click(screen.getByRole("link", { name: "Dashboard" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/"));
    expect(screen.queryByText("Discard unsaved changes?")).toBeNull();
  });

  it("asks before in-app navigation discards an edit, and Keep editing stays", async () => {
    const router = renderAt();
    await editPollInterval();
    fireEvent.click(screen.getByRole("link", { name: "Dashboard" }));
    expect(await screen.findByText("Discard unsaved changes?")).toBeTruthy();
    expect(router.state.location.pathname).toBe("/settings");

    fireEvent.click(screen.getByRole("button", { name: "Keep editing" }));
    await waitFor(() => expect(screen.queryByText("Discard unsaved changes?")).toBeNull());
    expect(router.state.location.pathname).toBe("/settings");
    expect((screen.getByLabelText("Poll interval") as HTMLInputElement).value).toBe("90s");
  });

  it("leaves once the edit is discarded", async () => {
    const router = renderAt();
    await editPollInterval();
    fireEvent.click(screen.getByRole("link", { name: "Dashboard" }));
    fireEvent.click(await screen.findByRole("button", { name: "Discard changes" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/"));
  });
});

describe("Settings memory profile", () => {
  /** Answers the heap endpoint with `heap()` and every other path as the default stub does. */
  function stubHeap(heap: () => Response) {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
      const path = String(input).split("?")[0];
      if (path === "/api/config") return json(200, CONFIG);
      if (path === "/api/diagnostics/heap") return heap();
      return json(404, { error: "not found" });
    });
    vi.stubGlobal("fetch", fetchMock);
    return fetchMock;
  }

  it("posts with the CSRF header and downloads heap-<timestamp>.pprof", async () => {
    document.cookie = "XSRF-TOKEN=tok123";
    const fetchMock = stubHeap(() => new Response(new Uint8Array([0x1f, 0x8b]), { status: 200 }));
    // jsdom implements neither object URLs nor anchor-driven downloads.
    URL.createObjectURL = vi.fn(() => "blob:heap");
    URL.revokeObjectURL = vi.fn();
    const downloads: string[] = [];
    const click = vi
      .spyOn(HTMLAnchorElement.prototype, "click")
      .mockImplementation(function (this: HTMLAnchorElement) {
        downloads.push(this.download);
      });

    renderAt();
    fireEvent.click(await screen.findByRole("button", { name: "Capture memory profile" }));

    await waitFor(() => expect(downloads).toHaveLength(1));
    expect(downloads[0]).toMatch(/^heap-\d{8}-\d{6}\.pprof$/);
    const call = fetchMock.mock.calls.find(([input]) => String(input) === "/api/diagnostics/heap");
    expect(call?.[1]?.method).toBe("POST");
    expect((call?.[1]?.headers as Record<string, string>)["X-CSRF-Token"]).toBe("tok123");
    click.mockRestore();
  });

  it("shows the server's error and re-enables the button", async () => {
    stubHeap(() => json(500, { error: "could not capture the heap profile" }));

    renderAt();
    fireEvent.click(await screen.findByRole("button", { name: "Capture memory profile" }));

    const message = await screen.findByText("could not capture the heap profile");
    expect(message.closest("[role=alert]")).toBeTruthy();
    await waitFor(() =>
      expect(
        (screen.getByRole("button", { name: "Capture memory profile" }) as HTMLButtonElement)
          .disabled,
      ).toBe(false),
    );
  });
});
