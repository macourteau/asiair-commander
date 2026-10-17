/**
 * Button — the primary action element. Renders a real <button>. Variants:
 * primary (accent), secondary (bordered), ghost (borderless), danger (destructive).
 *
 * Props: variant, size ("md" | "sm"), block (full width), loading (shows a
 * spinner and disables), plus all native <button> props. Provide a leading/
 * trailing icon via the `icon`/`iconAfter` slots.
 *
 *   <Button variant="primary" icon={<Icon name="scan" />}>Scan network</Button>
 */
import type { ButtonHTMLAttributes, ReactNode } from "react";
import { Spinner } from "./Spinner";

export type ButtonVariant = "primary" | "secondary" | "ghost" | "danger";

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: "md" | "sm";
  block?: boolean;
  loading?: boolean;
  icon?: ReactNode;
  iconAfter?: ReactNode;
}

export function Button({
  variant = "secondary",
  size = "md",
  block = false,
  loading = false,
  icon,
  iconAfter,
  disabled,
  children,
  className = "",
  type = "button",
  ...rest
}: ButtonProps) {
  const classes = [
    "aac-btn",
    `aac-btn--${variant}`,
    size === "sm" ? "aac-btn--sm" : "",
    block ? "aac-btn--block" : "",
    className,
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <button
      type={type}
      className={classes}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      {...rest}
    >
      {loading ? <Spinner size={16} /> : icon}
      {children}
      {!loading && iconAfter}
    </button>
  );
}
