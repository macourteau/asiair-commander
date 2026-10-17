/**
 * RigActions — the pause/resume/run-now/stop-run button cluster on a rig's
 * Devices card. Extracted from RigItem so the requested-vs-current button logic
 * (which one shows, and when it reads busy) is testable directly, without the
 * SSE snapshot context RigItem reads its rig status from.
 */
import type { RequestedState } from "@/api";
import { Button, Icon } from "@/design";

export interface RigActionsProps {
  /** The worker's published `paused` flag — stays true throughout a run, not only
   * while suspended. */
  paused: boolean;
  /** What the operator last asked for, recorded by the hub the instant a command
   * is queued. */
  requested: RequestedState;
  /** A Run now on a paused rig is running until idle; set for exactly its
   * duration. */
  runUntilIdle: boolean;
  /** Which command's request is in flight, or null. Drives the loading spinners. */
  busy: string | null;
  onPause: () => void;
  onResume: () => void;
  onRunNow: () => void;
  onStopRun: () => void;
}

export function RigActions({
  paused,
  requested,
  runUntilIdle,
  busy,
  onPause,
  onResume,
  onRunNow,
  onStopRun,
}: RigActionsProps) {
  // A pause queued but not yet applied: offer to take it back rather than a
  // second Pause that would queue another one. Never true during a run — Stop
  // run and Resume cover that case instead.
  const pausePending = !paused && !runUntilIdle && requested === "paused";
  // The run flag is still set, but a stop has already been requested: the
  // in-flight file finishes, then the run ends on its own.
  const stopPending = runUntilIdle && requested === "paused";

  return (
    <>
      {pausePending ? (
        <Button
          size="sm"
          variant="secondary"
          icon={<Icon name="play" size={16} />}
          loading={busy === "resume"}
          onClick={onResume}
        >
          Cancel pause
        </Button>
      ) : paused ? (
        <Button
          size="sm"
          variant="secondary"
          icon={<Icon name="play" size={16} />}
          loading={busy === "resume"}
          onClick={onResume}
        >
          Resume
        </Button>
      ) : (
        <Button
          size="sm"
          variant="secondary"
          icon={<Icon name="pause" size={16} />}
          loading={busy === "pause"}
          onClick={onPause}
        >
          Pause
        </Button>
      )}
      {runUntilIdle &&
        (stopPending ? (
          <Button size="sm" variant="secondary" disabled loading>
            Stopping…
          </Button>
        ) : (
          <Button
            size="sm"
            variant="secondary"
            icon={<Icon name="pause" size={16} />}
            loading={busy === "stop"}
            onClick={onStopRun}
          >
            Stop run
          </Button>
        ))}
      {!runUntilIdle && (
        <Button
          size="sm"
          variant="secondary"
          icon={<Icon name="run-now" size={16} />}
          loading={busy === "run"}
          onClick={onRunNow}
        >
          Run now
        </Button>
      )}
    </>
  );
}
