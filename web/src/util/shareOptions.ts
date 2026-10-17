/**
 * shareOptions builds the list a watched-share selector offers.
 *
 * Two sources feed it: what the device's share probe returned, and the names
 * already on the form. Both matter — the probe is the answer to "what shares
 * does this thing even have", but a rig can be configured while it is switched
 * off, so a hand-typed name must never vanish from the list just because no
 * probe has run or the probe failed.
 *
 * SMB share names are case-insensitive, so the same share spelled two ways is
 * one option. The device's spelling wins: it is the authority on its own names.
 */
export function shareOptions(
  probed: readonly string[],
  configured: readonly string[],
): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const name of [...probed, ...configured]) {
    const trimmed = name.trim();
    if (!trimmed) continue;
    const key = trimmed.toLowerCase();
    if (seen.has(key)) continue;
    seen.add(key);
    out.push(trimmed);
  }
  return out;
}

/**
 * probeOutcome phrases what a finished share probe found. "Answered but listed
 * nothing" is its own case: it is not a failure, but it is also not a result the
 * operator can pick from, so it has to say what to do next.
 */
export function probeOutcome(shares: readonly string[]): string {
  if (shares.length === 0) {
    return "That device answered but listed no shares. Type the share name by hand.";
  }
  const plural = shares.length === 1 ? "" : "s";
  return `Found ${shares.length} share${plural} on this device.`;
}
