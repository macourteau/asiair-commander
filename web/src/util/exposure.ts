/**
 * Renders an EXPTIME value the way the `<EXPOSURE>` token does (design
 * routing-and-filing §3.3): rounded to the millisecond first, because the FITS
 * value carries float noise (1.325 arrives as 1.32500004768372), then the
 * shortest decimal plus "s". Keeping the two in step means the UI shows the same
 * exposure the folder names.
 */
export function formatExposure(exptime: number): string {
  const secs = Math.round(exptime * 1000) / 1000;
  return `${String(secs)}s`;
}
