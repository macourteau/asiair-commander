/**
 * The pieces a rig card adds when the rig is reached through a remote: the
 * "remote" pill beside its name, and the lines its remote's state puts on the card
 * (design remote-sites §10). Shared by the
 * Dashboard card and the Devices list so both read the same.
 */
import type { RigStatus } from "@/api";
import { Icon, StatusPill } from "@/design";
import { isRemote, refusedForwardText, rigRemoteView } from "../util/remoteRig";

/** RemotePill marks a rig or found device reached through a remote. */
export function RemotePill({ site, remoteName }: { site?: string; remoteName?: string }) {
  if (!isRemote({ site })) return null;
  return (
    <span title={remoteName ? `Reached through the remote ${remoteName}` : "Reached through a remote"}>
      <StatusPill tone="info" icon="share" label="remote" />
    </span>
  );
}

/**
 * RigRemoteLines are the rig card's remote lines: the remote's state while the
 * card reads it instead of the rig's, the rig's last known verdict, the "via X
 * (reconnecting)" line over a live session, and any forward its remote refused.
 */
export function RigRemoteLines({ rig }: { rig: RigStatus }) {
  const view = rigRemoteView(rig);
  const refused = rig.refused_forwards ?? [];
  const latest = refused[refused.length - 1];
  if (!view.line && !view.via && !latest) return null;
  return (
    <div className="rig-remote-lines">
      {view.line && (
        <span role="status">
          <strong>{view.line}</strong>
        </span>
      )}
      {view.lastKnown && <span>{view.lastKnown}</span>}
      {view.via && <span>{view.via}</span>}
      {latest && (
        <span className="notice tone-problem">
          <Icon name="problem" size={15} />
          <span>
            {refusedForwardText(latest)}
            {refused.length > 1 ? ` (and ${refused.length - 1} earlier)` : ""}
          </span>
        </span>
      )}
    </div>
  );
}
