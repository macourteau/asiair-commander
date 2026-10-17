/**
 * Spinner — a busy indicator. Motion is decorative (reduced-motion stops it); the
 * meaning is carried by the accessible label. Inline in a button, or standalone
 * with a visible `label`.
 *
 *   <Spinner label="Loading rigs…" />
 */
export interface SpinnerProps {
  size?: number;
  /** Visible label rendered beside the spinner; also its accessible name. */
  label?: string;
  className?: string;
}

export function Spinner({ size = 20, label, className = "" }: SpinnerProps) {
  const ring = (
    <span
      className="aac-spinner"
      style={{ width: size, height: size }}
      aria-hidden={label ? true : undefined}
      role={label ? undefined : "status"}
      aria-label={label ? undefined : "Loading"}
    />
  );
  if (!label) return className ? <span className={className}>{ring}</span> : ring;
  return (
    <span
      className={`cluster ${className}`}
      role="status"
      style={{ ["--cluster-gap" as string]: "var(--space-2)" }}
    >
      {ring}
      <span className="text-muted text-sm">{label}</span>
    </span>
  );
}
