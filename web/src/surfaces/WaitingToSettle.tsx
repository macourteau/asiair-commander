/**
 * Plain-language "still saving" liveness line for frames the device has produced
 * but that haven't finished being written yet (not past the settle/quiescence
 * gate). It reassures the operator that Commander sees the new frames and is
 * deliberately waiting for them to finish, rather than reading as idle. Renders
 * nothing when nothing is waiting.
 */
export function WaitingToSettle({ count }: { count?: number }) {
  if (!count || count < 1) return null;
  const text =
    count === 1
      ? "1 new frame — waiting for it to finish saving"
      : `Waiting on ${count} new frames to finish saving`;
  return <p className="text-sm text-muted">{text}</p>;
}
