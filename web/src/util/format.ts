/**
 * Formatting helpers for machine data shown in the UI. Byte sizes, relative
 * times, and unix-seconds timestamps — always rendered in a `.mono` context.
 */

/** Human-readable bytes: 1536 → "1.5 KB", 0 → "0 B". Binary (1024) units. */
export function formatBytes(bytes: number, digits = 1): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const value = bytes / Math.pow(1024, i);
  const rounded = i === 0 ? String(bytes) : value.toFixed(digits);
  return `${rounded} ${units[i]}`;
}

/** Human-readable transfer rate: 13_000_000 → "12.4 MB/s". 0 → "0 B/s". */
export function formatRate(bytesPerSec: number): string {
  return `${formatBytes(bytesPerSec)}/s`;
}

/** Relative time from a unix-seconds timestamp, e.g. "2 min ago", "just now". */
export function relativeTime(unixSeconds: number, now = Date.now()): string {
  if (!unixSeconds) return "never";
  const diffMs = now - unixSeconds * 1000;
  const abs = Math.abs(diffMs);
  const sec = Math.round(abs / 1000);
  const future = diffMs < 0;
  const fmt = (n: number, unit: string) =>
    future ? `in ${n} ${unit}${n === 1 ? "" : "s"}` : `${n} ${unit}${n === 1 ? "" : "s"} ago`;
  if (sec < 45) return future ? "shortly" : "just now";
  const min = Math.round(sec / 60);
  if (min < 60) return fmt(min, "min");
  const hr = Math.round(min / 60);
  if (hr < 24) return fmt(hr, "hour");
  const day = Math.round(hr / 24);
  return fmt(day, "day");
}

/**
 * Compact process uptime from a seconds count, two largest units once past an
 * hour: 42 → "42s", 300 → "5m", 11520 → "3h 12m", 187200 → "2d 4h". Negative or
 * NaN clamps to "0s".
 */
export function formatUptime(seconds: number): string {
  const total = Number.isFinite(seconds) ? Math.max(0, Math.floor(seconds)) : 0;
  const d = Math.floor(total / 86400);
  const h = Math.floor((total % 86400) / 3600);
  const m = Math.floor((total % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m`;
  return `${total % 60}s`;
}

/** Absolute local time from unix seconds, e.g. "Jul 16, 22:14". Empty → "—". */
export function absoluteTime(unixSeconds: number): string {
  if (!unixSeconds) return "—";
  return new Date(unixSeconds * 1000).toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

/**
 * Countdown text for a next-frame ETA from a signed remaining-seconds value: a
 * future ETA reads "0:42" (m:ss), zero reads "due now", and a past ETA — overhead
 * ran long, which is normal, not expiry — reads "overdue 15s", rolling to
 * "overdue 1:30" once past a minute so a long stall stays readable.
 */
export function formatCountdown(remainingSec: number): string {
  const s = Math.round(remainingSec);
  if (s === 0) return "due now";
  const mmss = (n: number) => `${Math.floor(n / 60)}:${String(n % 60).padStart(2, "0")}`;
  if (s > 0) return mmss(s);
  const over = -s;
  return over < 60 ? `overdue ${over}s` : `overdue ${mmss(over)}`;
}

const pad2 = (n: number) => String(n).padStart(2, "0");

/**
 * Right ascension in degrees → "HHhMMmSSs" (e.g. 161.265 → "10h45m04s"). The
 * seconds are rounded to a whole number and carried up through minutes/hours so a
 * value like 59.6s never renders as "60s". Non-finite input → "—".
 */
export function formatRA(deg: number): string {
  if (!Number.isFinite(deg)) return "—";
  const d = ((deg % 360) + 360) % 360;
  const hours = d / 15;
  let h = Math.floor(hours);
  let m = Math.floor((hours - h) * 60);
  let s = Math.round((hours - h - m / 60) * 3600);
  if (s >= 60) {
    s -= 60;
    m += 1;
  }
  if (m >= 60) {
    m -= 60;
    h += 1;
  }
  if (h >= 24) h -= 24;
  return `${pad2(h)}h${pad2(m)}m${pad2(s)}s`;
}

/**
 * Declination in degrees → "±DD°MM′SS″" (e.g. -59.684 → "-59°41′04″"), with the
 * same 60-boundary carry as formatRA. Non-finite input → "—".
 */
export function formatDec(deg: number): string {
  if (!Number.isFinite(deg)) return "—";
  const sign = deg < 0 ? "-" : "+";
  const a = Math.abs(deg);
  let d = Math.floor(a);
  let m = Math.floor((a - d) * 60);
  let s = Math.round((a - d - m / 60) * 3600);
  if (s >= 60) {
    s -= 60;
    m += 1;
  }
  if (m >= 60) {
    m -= 60;
    d += 1;
  }
  return `${sign}${pad2(d)}°${pad2(m)}′${pad2(s)}″`;
}

/** Arcsec-per-pixel plate scale, e.g. 1.234 → "1.23″/px". Non-finite → "—". */
export function formatArcsecPerPx(v: number): string {
  if (!Number.isFinite(v)) return "—";
  return `${v.toFixed(2)}″/px`;
}

/** Angle in degrees → "12.3°". digits defaults to 1. Non-finite → "—". */
export function formatDegrees(v: number, digits = 1): string {
  if (!Number.isFinite(v)) return "—";
  return `${v.toFixed(digits)}°`;
}

/** Just the leaf filename of a share/source path. */
export function basename(path: string): string {
  if (!path) return "";
  const parts = path.split(/[\\/]/);
  return parts[parts.length - 1] || path;
}
