/**
 * StuckFrameDialog — what to do about one frame that can't be removed from the
 * device automatically (History's never-deletable view, which the backlog alert
 * links to). It says in plain words why the frame stays and what clears it, keeps
 * the recorded technical reason behind a disclosure, and offers only the actions
 * that exist for the frame's state:
 *
 * - unclassified: Dismiss (the existing `DELETE /api/rigs/{id}/frames/{frameId}`,
 *   design frame-lifecycle §13), which removes Commander's quarantine copy from
 *   the NAS and never touches the device file; and a link to the rig's
 *   Unclassified frames panel in Devices, where Reclassify lives.
 * - failed and held: no action. A failed frame retries on its own; a held frame
 *   has no operator exit by design (design delete-safety §2.4), and the dialog
 *   says so rather than offering one.
 */
import { useState } from "react";
import { Link } from "react-router";
import { api, ApiError, type Frame } from "@/api";
import { Button, Dialog, useToast } from "@/design";
import { basename } from "../util/format";
import { stuckFrameHelp } from "../util/stuckFrame";
import { UNCLASSIFIED_PANEL, devicesPanelLink } from "../util/alerts";

export function StuckFrameDialog({
  frame,
  rigLabel,
  onClose,
  onDismissed,
}: {
  frame: Frame | null;
  rigLabel: string;
  onClose: () => void;
  /** Called after a successful dismiss, so the list can reload. */
  onDismissed: () => void;
}) {
  const toast = useToast();
  const [dismissing, setDismissing] = useState(false);
  if (!frame) return null;
  const help = stuckFrameHelp(frame);
  if (!help) return null;
  const name = basename(frame.source_path) || frame.source_path;

  const dismiss = async () => {
    setDismissing(true);
    try {
      await api.discardFrame(frame.rig_id, frame.id);
      toast.success("Frame dismissed and cleared from the backlog.");
      onDismissed();
      onClose();
    } catch (err) {
      toast.error(err instanceof ApiError ? err.message : "Couldn't dismiss that frame.");
    } finally {
      setDismissing(false);
    }
  };

  return (
    <Dialog
      open
      onClose={onClose}
      title={`Why ${name} stays on ${rigLabel}`}
      footer={
        <>
          <Button variant={help.canDismiss ? "ghost" : "secondary"} onClick={onClose}>
            {help.canDismiss ? "Cancel" : "Close"}
          </Button>
          {help.canDismiss && (
            <Button variant="danger" loading={dismissing} onClick={dismiss}>
              Dismiss frame
            </Button>
          )}
        </>
      }
    >
      <p>{help.why}</p>
      <p>{help.next}</p>
      {frame.state === "unclassified" && (
        <>
          <p>
            <Link to={devicesPanelLink(frame.rig_id, UNCLASSIFIED_PANEL)}>
              Reclassify {rigLabel}'s frames in Devices
            </Link>
          </p>
          {help.canDismiss && (
            <p>
              <strong>Dismiss frame</strong> removes Commander's copy from the NAS and can't be
              undone. It does <strong>not</strong> delete the file from the device: that stays
              until you delete it yourself, and Commander won't copy it again unless it changes.
            </p>
          )}
        </>
      )}
      {frame.error && (
        <details>
          <summary>Technical detail</summary>
          <p className="mono break-anywhere">{frame.error}</p>
        </details>
      )}
    </Dialog>
  );
}
