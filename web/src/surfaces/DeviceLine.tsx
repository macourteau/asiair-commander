/**
 * The sentence of a rig's device line (util/drain.ts): what is still on the
 * device and why, with the reason nothing will clear in the problem tone. The
 * Dashboard card adds its badge and links around it; Devices shows it alone.
 */
import type { DeviceLinePart } from "../util/drain";
import "./device-line.css";

export function DeviceLineText({ parts }: { parts: DeviceLinePart[] }) {
  return (
    <span>
      {parts.map((p, i) =>
        p.problem ? (
          <span key={i} className="tone-problem device-line__problem">
            {p.text}
          </span>
        ) : (
          <span key={i}>{p.text}</span>
        ),
      )}
    </span>
  );
}
