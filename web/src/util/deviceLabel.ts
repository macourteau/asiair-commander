/**
 * Naming and describing a device found by a network scan.
 *
 * A scan_air reply carries no display name, so a label has to be derived. The
 * model string alone cannot do it: every ASIAIR of a generation reports the same
 * one ("Raspberry Pi Compute Module 4 Rev 1.0"), so an operator adding three
 * kits in a row sees three identical labels. The ssid — the device's own Wi-Fi
 * name — is the field its owner set and recognises, so it leads wherever a
 * device has to be told apart from its siblings.
 *
 * Everything here is pure so the wording and the fallbacks are pinned by tests
 * rather than only by a browser.
 */

/** The scan fields naming needs; a full DiscoveredRig satisfies it. */
export interface DeviceIdentity {
  guid: string;
  model: string;
  ssid: string;
}

/** shortGuid trims a guid to its first 8 characters for display. */
export function shortGuid(guid: string): string {
  return guid ? guid.slice(0, 8) : "unknown";
}

/** deviceLabel derives a label from the model plus a short guid. */
export function deviceLabel(d: DeviceIdentity): string {
  const short = shortGuid(d.guid);
  return d.model ? `${d.model} (${short})` : `ASIAIR ${short}`;
}

/** slug reduces a name to the stable key form used for rig ids. */
export function slug(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/(^-|-$)/g, "");
}

/**
 * rigNamePrefill picks the name to drop into the editor when adopting a found
 * device. The ssid wins when it yields a usable rig id: the derived id is a
 * stable key, so an ssid of punctuation alone ("!!!") would slug to nothing and
 * leave the operator with a blank id and a validation error instead of a head
 * start. In that case the model-plus-guid label is the safer prefill.
 */
export function rigNamePrefill(d: DeviceIdentity): string {
  const ssid = d.ssid.trim();
  if (ssid && slug(ssid)) return ssid;
  return deviceLabel(d);
}

/**
 * deviceFacts is the one-line identity strip shown beside a device's guid — the
 * supporting detail an operator uses to confirm they are configuring the kit
 * they think they are. Blank parts drop out rather than leaving stray
 * separators.
 */
export function deviceFacts(d: { ssid?: string; address?: string; model?: string }): string {
  return [d.ssid, d.address, d.model]
    .map((p) => (p ?? "").trim())
    .filter(Boolean)
    .join(" · ");
}
