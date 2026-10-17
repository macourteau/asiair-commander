/**
 * Card — the primary surface container. `pad` adds default padding; `interactive`
 * adds hover affordance (use when the whole card is clickable/expandable).
 * Render as any element via `as` (default <section>) so cards can be <article>,
 * <li>, etc. for correct document semantics.
 *
 *   <Card as="article" pad>…</Card>
 */
import type { ElementType, HTMLAttributes, ReactNode } from "react";

export interface CardProps extends HTMLAttributes<HTMLElement> {
  as?: ElementType;
  pad?: boolean;
  interactive?: boolean;
  children: ReactNode;
}

export function Card({
  as: Tag = "section",
  pad = false,
  interactive = false,
  className = "",
  children,
  ...rest
}: CardProps) {
  const classes = [
    "aac-card",
    pad ? "aac-card--pad" : "",
    interactive ? "aac-card--interactive" : "",
    className,
  ]
    .filter(Boolean)
    .join(" ");
  return (
    <Tag className={classes} {...rest}>
      {children}
    </Tag>
  );
}
