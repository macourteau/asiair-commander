/**
 * FrameThumb — a lazy-loaded thumbnail cell for frame lists. Renders a small
 * preview `<img>` for frames whose bytes are on disk (clicking opens the lightbox),
 * and a neutral placeholder for pending/failed/ineligible frames (no request made).
 * `loading="lazy"` is the load-bearing brake against a list page fetching every row
 * at once (most lists are not virtualized).
 */
import { useEffect, useState } from "react";
import { frameHasImage, frameImageUrl } from "@/api";
import type { Frame } from "@/api";
import { basename } from "../util/format";

export function FrameThumb({
  frame,
  onOpen,
}: {
  frame: Frame | null | undefined;
  onOpen?: (frame: Frame) => void;
}) {
  const [failed, setFailed] = useState(false);

  // Reset the error state when the row's underlying frame changes (list reuse).
  useEffect(() => setFailed(false), [frame?.id, frame?.sha256]);

  if (!frame || !frameHasImage(frame) || failed) {
    return <span className="frame-thumb frame-thumb--empty" aria-hidden="true" />;
  }
  return (
    <button
      type="button"
      className="frame-thumb"
      onClick={() => onOpen?.(frame)}
      aria-label={`Preview ${basename(frame.source_path)}`}
    >
      <img
        loading="lazy"
        src={frameImageUrl(frame, "thumb")}
        alt=""
        onError={() => setFailed(true)}
      />
    </button>
  );
}
