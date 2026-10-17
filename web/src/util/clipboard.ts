/**
 * copyText writes s to the clipboard, falling back to a hidden textarea +
 * execCommand when navigator.clipboard is unavailable — Commander is commonly
 * served over plain http://<nas>:8686, a non-secure context where the async
 * Clipboard API is blocked.
 */
export async function copyText(s: string): Promise<void> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(s);
      return;
    }
  } catch {
    // Blocked (non-secure context): fall through to the legacy path.
  }
  const ta = document.createElement("textarea");
  ta.value = s;
  ta.style.position = "fixed";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  try {
    document.execCommand("copy");
  } finally {
    document.body.removeChild(ta);
  }
}

/** base64Bytes decodes standard base64, as Go's encoding/base64 writes it. */
export function base64Bytes(b64: string): Uint8Array<ArrayBuffer> {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/** downloadBlob saves blob as a file named name, without a round trip to the server. */
export function downloadBlob(name: string, blob: Blob): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  // Revoked on a later tick: some browsers (Firefox, Safari) read the blob after
  // click() returns, and an immediate revoke cancels their download.
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
