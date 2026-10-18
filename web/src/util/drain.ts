/**
 * How a rig card reads the server's `drain` summary (model.RigStatus.Drain):
 * "drained", a count such as "3 resident", or "unknown" from startup until the
 * first listing of the device completes. (An empty summary hides the row, so it
 * never reaches here.) An unknown count is not a count: the
 * card must not put it in a sentence or claim the frames are in their grace
 * period.
 */
export type DrainView =
  | { kind: "drained" }
  | { kind: "unchecked" }
  | { kind: "count"; text: string };

export function drainView(drain: string): DrainView {
  const d = drain.trim().toLowerCase();
  if (d === "drained") return { kind: "drained" };
  if (d === "unknown") return { kind: "unchecked" };
  return { kind: "count", text: drain.trim() };
}
