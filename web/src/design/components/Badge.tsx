/**
 * Badge — a small count/label chip (e.g. the unacked-alert count in the nav).
 * Variant "problem" tints it red for attention; "accent" for neutral highlight.
 * When it's a live count, pass an `aria-label` so screen readers get context
 * ("3 unacknowledged alerts") rather than a bare number.
 */
import type { HTMLAttributes, ReactNode } from "react";

export interface BadgeProps extends HTMLAttributes<HTMLSpanElement> {
  variant?: "neutral" | "problem" | "accent";
  children: ReactNode;
}

export function Badge({
  variant = "neutral",
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
      {children}
    </span>
  );
}
