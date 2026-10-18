/**
 * Inline SVG icon set. Icons are decorative by default (aria-hidden) — the
 * accessible name comes from the surrounding control's label/aria-label. Pass a
 * `title` to make a standalone icon announce itself.
 *
 * All glyphs are drawn on a 24x24 viewBox with `currentColor` stroke/fill, so
 * they inherit text color and the status/accent tokens automatically.
 */
import type { SVGProps } from "react";

export type IconName =
  | "safe" // check — safe to power off / verified
  | "working" // filled dot — in progress
  | "problem" // alert triangle — problem/error
  | "unknown" // question — unknowable / offline
  | "info"
  | "rig" // the telescope/device
  | "alert" // bell
  | "settings" // gear
  | "logs" // scroll/lines
  | "devices" // server stack
  | "dashboard" // grid
  | "share" // folder/share storage
  | "nas" // database (destination)
  | "menu"
  | "close"
  | "chevron-down"
  | "chevron-right"
  | "sun"
  | "moon"
  | "logout"
  | "play" // resume
  | "pause"
  | "run-now" // refresh/lightning
  | "scan" // search/radar
  | "add"
  | "trash"
  | "edit"
  | "reclassify"
  | "update" // two circling arrows — a newer release is available
  | "external"
  | "download" // tray with a down arrow — save a file to disk
  | "upload" // tray with an up arrow — send a file from this computer
  | "rename" // text cursor beside a line — rename an item in place
  | "feed" // activity pulse — the live transfer feed
  | "missing" // slashed circle — a filed frame gone missing from the NAS
  | "clock"
  | "tools" // wrench — the Tools page
  | "folder" // closed folder — a device folder in the file browser
  | "file"; // page with a folded corner — a device file

type Path = { d: string; fill?: boolean };

const CIRCLING_ARROWS: Path[] = [
  { d: "M4 12a8 8 0 0 1 13.7-5.6L20 8" },
  { d: "M20 4v4h-4" },
  { d: "M20 12a8 8 0 0 1-13.7 5.6L4 16" },
  { d: "M4 20v-4h4" },
];

// Stroke-based glyphs unless `fill: true`. Kept deliberately simple/legible at
// 16-20px, the sizes the UI uses most.
const ICONS: Record<IconName, Path[]> = {
  safe: [{ d: "M20 6 9 17l-5-5" }],
  working: [{ d: "M12 12m-5 0a5 5 0 1 0 10 0a5 5 0 1 0 -10 0", fill: true }],
  problem: [
    { d: "M12 3 2 20h20L12 3Z" },
    { d: "M12 10v4" },
    { d: "M12 17.5v.5", fill: false },
  ],
  unknown: [
    { d: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z" },
    { d: "M9.2 9.5a2.8 2.8 0 0 1 5.5.7c0 1.9-2.7 2.4-2.7 4" },
    { d: "M12 17.5v.5" },
  ],
  info: [
    { d: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z" },
    { d: "M12 11v5" },
    { d: "M12 7.5v.5" },
  ],
  rig: [
    { d: "M4 15.5 14.5 5 19 9.5 8.5 20 4 15.5Z" },
    { d: "M13 6.5 17.5 11" },
    { d: "M11 22h6" },
    { d: "M14 22l-2-3" },
  ],
  alert: [
    { d: "M18 9a6 6 0 1 0-12 0c0 6-2.5 7-2.5 7h17S18 15 18 9Z" },
    { d: "M10.5 20a2 2 0 0 0 3 0" },
  ],
  settings: [
    { d: "M12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7Z" },
    {
      d: "M19.4 13a1.6 1.6 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.6 1.6 0 0 0-2.7 1.1V21a2 2 0 1 1-4 0v-.2A1.6 1.6 0 0 0 6.6 19l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1A1.6 1.6 0 0 0 3 13H3a2 2 0 1 1 0-4h.1A1.6 1.6 0 0 0 4.6 6.4l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1A1.6 1.6 0 0 0 10 4.6V4a2 2 0 1 1 4 0v.1a1.6 1.6 0 0 0 2.7 1.1l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.6 1.6 0 0 0-.3 1.8v.1a1.6 1.6 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.6 1.6 0 0 0-1.5 1Z" },
  ],
  logs: [
    { d: "M5 4h14v16H5z" },
    { d: "M8 8h8" },
    { d: "M8 12h8" },
    { d: "M8 16h5" },
  ],
  devices: [
    { d: "M4 5h16v5H4z" },
    { d: "M4 14h16v5H4z" },
    { d: "M7.5 7.5v.01" },
    { d: "M7.5 16.5v.01" },
  ],
  dashboard: [
    { d: "M4 4h7v7H4z" },
    { d: "M13 4h7v4h-7z" },
    { d: "M13 11h7v9h-7z" },
    { d: "M4 13h7v7H4z" },
  ],
  share: [{ d: "M4 7h5l2 2h9v9a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2Z" }],
  nas: [
    { d: "M12 8c4.4 0 8-1.3 8-3s-3.6-3-8-3-8 1.3-8 3 3.6 3 8 3Z" },
    { d: "M4 5v14c0 1.7 3.6 3 8 3s8-1.3 8-3V5" },
    { d: "M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3" },
  ],
  menu: [{ d: "M4 7h16" }, { d: "M4 12h16" }, { d: "M4 17h16" }],
  close: [{ d: "M6 6l12 12" }, { d: "M18 6 6 18" }],
  "chevron-down": [{ d: "m6 9 6 6 6-6" }],
  "chevron-right": [{ d: "m9 6 6 6-6 6" }],
  sun: [
    { d: "M12 16a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z" },
    { d: "M12 2v2M12 20v2M4 12H2M22 12h-2M5 5 4 4M20 20l-1-1M19 5l1-1M4 20l1-1" },
  ],
  moon: [{ d: "M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5Z" }],
  logout: [
    { d: "M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" },
    { d: "M16 17l5-5-5-5" },
    { d: "M21 12H9" },
  ],
  play: [{ d: "M7 4.5v15l13-7.5-13-7.5Z", fill: true }],
  pause: [{ d: "M8 4.5h3v15H8z", fill: true }, { d: "M13 4.5h3v15h-3z", fill: true }],
  "run-now": [
    { d: "M20 12a8 8 0 1 1-2.3-5.6" },
    { d: "M20 4v4h-4" },
  ],
  scan: [
    { d: "M11 18a7 7 0 1 0 0-14 7 7 0 0 0 0 14Z" },
    { d: "m21 21-4.5-4.5" },
  ],
  add: [{ d: "M12 5v14" }, { d: "M5 12h14" }],
  trash: [
    { d: "M4 7h16" },
    { d: "M9 7V5a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2" },
    { d: "M6 7l1 13a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-13" },
  ],
  edit: [
    { d: "M4 20h4L19 9l-4-4L4 16v4Z" },
    { d: "M14 6l4 4" },
  ],
  reclassify: CIRCLING_ARROWS,
  // Same glyph, own name, so the newer-release link says what it means.
  update: CIRCLING_ARROWS,
  external: [
    { d: "M14 4h6v6" },
    { d: "M20 4 10 14" },
    { d: "M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5" },
  ],
  download: [
    { d: "M12 3v11" },
    { d: "M8 10.5 12 14l4-3.5" },
    { d: "M4 17v2a1 1 0 0 0 1 1h14a1 1 0 0 0 1-1v-2" },
  ],
  upload: [
    { d: "M12 14V3" },
    { d: "M8 6.5 12 3l4 3.5" },
    { d: "M4 17v2a1 1 0 0 0 1 1h14a1 1 0 0 0 1-1v-2" },
  ],
  rename: [
    { d: "M4 8h9a1 1 0 0 1 1 1v6a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V9a1 1 0 0 1 1-1Z" },
    { d: "M18 5v14" },
    { d: "M16 5h4" },
    { d: "M16 19h4" },
  ],
  clock: [
    { d: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z" },
    { d: "M12 7.5V12l3 2" },
  ],
  feed: [{ d: "M3 12h4l2.5-7 4 14 2.5-7H21" }],
  missing: [
    { d: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18Z" },
    { d: "M5.6 5.6l12.8 12.8" },
  ],
  folder: [{ d: "M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7Z" }],
  file: [
    { d: "M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8l-5-5Z" },
    { d: "M14 3v5h5" },
  ],
  tools: [
    {
      d: "M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76Z",
    },
  ],
};

export interface IconProps extends Omit<SVGProps<SVGSVGElement>, "name"> {
  name: IconName;
  /** Pixel size (width and height). Defaults to 20 — one grid step below the 24 canvas. */
  size?: number;
  /** When set, the icon is announced (role=img) with this label instead of being decorative. */
  title?: string;
}

export function Icon({ name, size = 20, title, ...rest }: IconProps) {
  const paths = ICONS[name];
  const labelled = title != null;
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.75}
      strokeLinecap="round"
      strokeLinejoin="round"
      role={labelled ? "img" : undefined}
      aria-hidden={labelled ? undefined : true}
      aria-label={labelled ? title : undefined}
      {...rest}
    >
      {title != null && <title>{title}</title>}
      {paths.map((p, i) =>
        p.fill ? (
          <path key={i} d={p.d} fill="currentColor" stroke="none" />
        ) : (
          <path key={i} d={p.d} />
        ),
      )}
    </svg>
  );
}
