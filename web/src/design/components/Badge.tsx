/**
 * Badge — a small count/label chip (e.g. the unacked-alert count in the nav).
 * Variant "problem" tints it red for attention; "accent" for neutral highlight.
 * When it's a live count, pass `srLabel` so screen readers get context
 * ("3 unacknowledged alerts") rather than a bare number: the visible text is
 * hidden from them and the label rendered as visually-hidden text, since an
 * aria-label on a plain span is not reliably read.
 */
import type { HTMLAttributes, ReactNode } from "react";

export interface BadgeProps extends HTMLAttributes<HTMLSpanElement> {
  variant?: "neutral" | "problem" | "accent";
  /** Screen-reader wording that replaces the visible text. */
  srLabel?: string;
  children: ReactNode;
}

export function Badge({
  variant = "neutral",
  srLabel,
  className = "",
  children,
  ...rest
}: BadgeProps) {
  const classes = [
    "aac-badge",
    variant !== "neutral" ? `aac-badge--${variant}` : "",
    className,
  ]
    .filter(Boolean)
    .join(" ");
  return (
    <span className={classes} {...rest}>
      {srLabel ? (
        <>
          <span aria-hidden="true">{children}</span>
          <span className="sr-only">{srLabel}</span>
        </>
      ) : (
        children
      )}
    </span>
  );
}
