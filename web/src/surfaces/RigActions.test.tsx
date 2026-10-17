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
      />,
    );
    expect(screen.getByText("Resume")).toBeTruthy();
    expect(screen.getByText("Stop run")).toBeTruthy();
    expect(screen.queryByText("Run now")).toBeNull();
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
      />,
    );
    const btn = screen.getByText("Stopping…").closest("button");
    expect(btn?.disabled).toBe(true);
    expect(screen.queryByText("Stop run")).toBeNull();
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
      />,
    );
    expect(screen.getByText("Resume")).toBeTruthy();
    expect(screen.getByText("Run now")).toBeTruthy();
    expect(screen.queryByText("Stop run")).toBeNull();
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
      />,
    );
    const btn = screen.getByText("Cancel pause");
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
      />,
    );
    expect(screen.getByText("Pause")).toBeTruthy();
    expect(screen.getByText("Run now")).toBeTruthy();
  });
});
