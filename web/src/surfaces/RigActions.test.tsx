// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { RigActions } from "./RigActions";

afterEach(cleanup);

const noop = () => {};

describe("RigActions", () => {
  it("offers Stop run beside Resume while a run is in progress", () => {
    render(
      <RigActions
        paused
        requested="run_until_idle"
        runUntilIdle
        busy={null}
        onPause={noop}
        onResume={noop}
        onRunNow={noop}
        onStopRun={noop}
        rigName="Alpha"
      />,
    );
    expect(screen.getByRole("button", { name: "Resume copying from Alpha" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Stop run on Alpha" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Run now on Alpha" })).toBeNull();
  });

  it("disables Stop run and reads Stopping… while a stop is pending", () => {
    render(
      <RigActions
        paused
        requested="paused"
        runUntilIdle
        busy={null}
        onPause={noop}
        onResume={noop}
        onRunNow={noop}
        onStopRun={noop}
        rigName="Alpha"
      />,
    );
    const btn = screen.getByText("Stopping…").closest("button");
    expect(btn?.disabled).toBe(true);
    expect(screen.queryByRole("button", { name: "Stop run on Alpha" })).toBeNull();
  });

  it("shows Paused with no Stop run once the run flag clears", () => {
    render(
      <RigActions
        paused
        requested="paused"
        runUntilIdle={false}
        busy={null}
        onPause={noop}
        onResume={noop}
        onRunNow={noop}
        onStopRun={noop}
        rigName="Alpha"
      />,
    );
    expect(screen.getByRole("button", { name: "Resume copying from Alpha" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Run now on Alpha" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Stop run on Alpha" })).toBeNull();
    expect(screen.queryByText("Stopping…")).toBeNull();
  });

  it("offers Cancel pause while a pause is pending", () => {
    const onResume = vi.fn();
    render(
      <RigActions
        paused={false}
        requested="paused"
        runUntilIdle={false}
        busy={null}
        onPause={noop}
        onResume={onResume}
        onRunNow={noop}
        onStopRun={noop}
        rigName="Alpha"
      />,
    );
    const btn = screen.getByRole("button", { name: "Cancel pause for Alpha" });
    fireEvent.click(btn);
    expect(onResume).toHaveBeenCalledOnce();
  });

  it("offers plain Pause when nothing is pending and the rig is running", () => {
    render(
      <RigActions
        paused={false}
        requested="running"
        runUntilIdle={false}
        busy={null}
        onPause={noop}
        onResume={noop}
        onRunNow={noop}
        onStopRun={noop}
        rigName="Alpha"
      />,
    );
    expect(screen.getByRole("button", { name: "Pause copying from Alpha" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Run now on Alpha" })).toBeTruthy();
  });
});
