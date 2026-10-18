/**
 * IconButton — an icon-only control. Because it has no visible text, `label` is
 * REQUIRED and becomes the aria-label (and the native title tooltip), so the
 * control is never unlabeled for screen readers.
 *
 *   <IconButton label="Dismiss" icon={<Icon name="close" />} onClick={close} />
 */
import type { ButtonHTMLAttributes, ReactNode, Ref } from "react";

export interface IconButtonProps
  extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, "aria-label"> {
  /** Accessible name — required for an icon-only control. */
  label: string;
  icon: ReactNode;
  active?: boolean;
  ref?: Ref<HTMLButtonElement>;
}

export function IconButton({
  label,
  icon,
  active = false,
  className = "",
  type = "button",
  title,
  ...rest
}: IconButtonProps) {
  const classes = [
    "aac-iconbtn",
    active ? "aac-iconbtn--active" : "",
    className,
  ]
    .filter(Boolean)
    .join(" ");
  return (
    <button
      type={type}
      className={classes}
      aria-label={label}
      title={title ?? label}
      aria-pressed={active || undefined}
      {...rest}
    >
      {icon}
    </button>
  );
}
