/**
 * FrameLightbox — full-size preview viewer built on yet-another-react-lightbox.
 * Shows the 2048px `screen` render initially; on zoom-in the Zoom plugin loads the
 * native-resolution `full` render (via a srcSet built from the frame's dimensions)
 * so stars/focus can be inspected at 1:1. Download delivers `full`, and a custom
 * Copy button places the image on the clipboard (secure-context only).
 */
import { useEffect, useMemo, useState } from "react";
import Lightbox, { type SlideImage } from "yet-another-react-lightbox";
import Zoom from "yet-another-react-lightbox/plugins/zoom";
import Download from "yet-another-react-lightbox/plugins/download";
import "yet-another-react-lightbox/styles.css";
import { api, frameImageUrl } from "@/api";
import type { Frame } from "@/api";
import { basename } from "../util/format";

const SCREEN_MAX = 2048;

export function FrameLightbox({
  frame,
  onClose,
}: {
  frame: Frame | null;
  onClose: () => void;
}) {
  const canCopy =
    typeof navigator !== "undefined" &&
    typeof window !== "undefined" &&
    window.isSecureContext &&
    typeof navigator.clipboard?.write === "function";

  // Native dimensions drive the zoom srcSet; fetched once per frame. The lightbox
  // still opens immediately on the `screen` render if this is slow or fails.
  const [dims, setDims] = useState<{ width: number; height: number } | null>(null);
  useEffect(() => {
    setDims(null);
    if (!frame) return;
    const ctrl = new AbortController();
    api.imageMeta(frame.id, ctrl.signal).then(setDims).catch(() => {});
    return () => ctrl.abort();
  }, [frame?.id]);

  const slides = useMemo(() => {
    if (!frame) return [];
    const name = basename(frame.source_path).replace(/\.[^.]+$/, "") || "preview";
    const screen = frameImageUrl(frame, "screen");
    const full = frameImageUrl(frame, "full");
    const slide: SlideImage = {
      src: screen,
      downloadUrl: full,
      downloadFilename: `${name}.jpg`,
    };
    if (dims && dims.width > 0 && dims.height > 0) {
      const long = Math.max(dims.width, dims.height);
      const scale = long > SCREEN_MAX ? SCREEN_MAX / long : 1;
      const sw = Math.max(1, Math.round(dims.width * scale));
      const sh = Math.max(1, Math.round(dims.height * scale));
      slide.width = sw;
      slide.height = sh;
      slide.srcSet = [
        { src: screen, width: sw, height: sh },
        { src: full, width: dims.width, height: dims.height },
      ];
    }
    return [slide];
  }, [frame, dims]);

  if (!frame) return null;

  const buttons: React.ReactNode[] = [];
  if (canCopy)
    buttons.push(
      <CopyButton
        key="copy"
        full={frameImageUrl(frame, "full")}
        screen={frameImageUrl(frame, "screen")}
      />,
    );
  buttons.push("download", "close");

  return (
    <Lightbox
      open
      close={onClose}
      carousel={{ finite: true }}
      controller={{ closeOnBackdropClick: true }}
      plugins={[Zoom, Download]}
      slides={slides}
      toolbar={{ buttons }}
      render={{ buttonPrev: () => null, buttonNext: () => null }}
    />
  );
}

/**
 * CopyButton copies the image to the clipboard as PNG (the only format browsers
 * reliably accept). The ClipboardItem is constructed with a *promise* value
 * synchronously inside the click handler so the user activation survives the fetch
 * + canvas awaits (Safari drops it otherwise).
 */
function CopyButton({ full, screen }: { full: string; screen: string }) {
  const toPng = (url: string) =>
    fetch(url, { credentials: "include" })
      .then((r) => r.blob())
      .then((blob) => createImageBitmap(blob))
      .then((bmp) => {
        const canvas = document.createElement("canvas");
        canvas.width = bmp.width;
        canvas.height = bmp.height;
        const ctx = canvas.getContext("2d");
        if (!ctx) throw new Error("no 2d context");
        ctx.drawImage(bmp, 0, 0);
        return new Promise<Blob>((res, rej) =>
          canvas.toBlob(
            (b) => (b ? res(b) : rej(new Error("toBlob failed"))),
            "image/png",
          ),
        );
      });
  return (
    <button
      type="button"
      className="yarl__button"
      title="Copy image"
      onClick={() => {
        // Native-resolution first (the chosen "full size" for copy); if that render
        // is too large to fetch/encode, fall back to the on-screen 2048px image.
        const png = toPng(full).catch(() => toPng(screen));
        void navigator.clipboard
          .write([new ClipboardItem({ "image/png": png })])
          .catch(() => {
            /* clipboard write can fail on very large images or lost activation */
          });
      }}
    >
      <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true">
        <rect x="9" y="9" width="11" height="11" rx="2" />
        <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
      </svg>
    </button>
  );
}
