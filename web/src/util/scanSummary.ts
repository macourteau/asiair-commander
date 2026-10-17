/**
 * scanSummary phrases the toast a finished network scan raises.
 *
 * A scan returns the whole discovery registry, not the round's replies: the
 * registry keeps a device for ten minutes after it stops answering, and keeps a
 * managed one indefinitely so a configured rig survives being powered off.
 * Counting the list therefore reports devices that are switched off — only the
 * entries still inside the staleness window (`online`) answered.
 *
 * The set-up/not-set-up split is here because a bare count with an empty "Found
 * on your network" list underneath it reads as a contradiction.
 *
 * A device that answered is NAMED while few enough answered to read in one line,
 * because the count alone is what a scan cannot settle: an operator watching a
 * rig sit at Offline wants to know whether THAT rig replied. And an answering
 * managed device is described as reconnecting rather than "set up", since "set up"
 * reads as "everything is fine" beside a card still saying Offline. The toast has
 * no per-rig connection state to be more precise than that; the rig card is where
 * the outcome shows up.
 */
import type { DiscoveredRig, RigProfile } from "@/api";

/** Above this many answering devices, names stop fitting and counts read better. */
const NAME_LIMIT = 3;

export function scanSummary(found: DiscoveredRig[], rigs?: RigProfile[] | null): string {
  const answered = found.filter((d) => d.online);
  if (answered.length === 0) return "Scan finished — no devices answered.";

  const known = answered.filter((d) => d.managed);
  const fresh = answered.filter((d) => !d.managed);
  const head = `Scan finished — ${answered.length} device${answered.length === 1 ? "" : "s"} answered`;

  const parts: string[] = [];
  if (answered.length <= NAME_LIMIT) {
    const reconnecting = names(known, rigs);
    const unadopted = names(fresh, rigs);
    if (reconnecting.length > 0) {
      parts.push(`${list(reconnecting)} ${reconnecting.length === 1 ? "is" : "are"} reconnecting`);
    }
    if (unadopted.length > 0) {
      parts.push(`${list(unadopted)} ${unadopted.length === 1 ? "isn't" : "aren't"} set up yet`);
    }
  } else {
    if (known.length > 0) parts.push(`${known.length} reconnecting`);
    if (fresh.length > 0) parts.push(`${fresh.length} not set up yet`);
  }
  return `${head}: ${parts.join(", ")}.`;
}

/**
 * names labels each device, dropping repeats: a rig is bound by guid OR by
 * cpuId, so a device that changed guid can be answering alongside the entry it
 * replaced, and both carry its rig's name. Naming that rig twice reads as two
 * rigs. The count stays a device count, which is what answered.
 */
function names(devices: DiscoveredRig[], rigs?: RigProfile[] | null): string[] {
  return [...new Set(devices.map((d) => label(d, rigs)))];
}

/** list renders labels as "a", "a and b", or "a, b and c". */
function list(labels: string[]): string {
  const rest = labels.slice(0, -1);
  const last = labels[labels.length - 1] ?? "";
  return rest.length === 0 ? last : `${rest.join(", ")} and ${last}`;
}

/**
 * label names one device the way its owner would: a managed device by its rig's
 * name, anything else by the address it answered from. The rig list can be
 * absent (the page scans as it loads), and a device can be bound to a profile
 * whose name we don't have, hence the fallbacks.
 */
function label(d: DiscoveredRig, rigs?: RigProfile[] | null): string {
  const rig = d.rig_id ? rigs?.find((r) => r.id === d.rig_id) : undefined;
  return rig?.name || d.address || d.rig_id || d.guid;
}
