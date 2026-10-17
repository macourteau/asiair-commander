/**
 * StatusPill — a status shown as COLOR + ICON + TEXT together, never color alone
 * (the core accessibility rule). Pass either a ready-made StatusView (from
 * design/status.ts) via `view`, or an explicit tone/icon/label.
 *
 *   <StatusPill view={rigStateView(rig.state)} />
 *   <StatusPill tone="problem" icon="problem" label="Error" size="lg" />
 */
import { Icon, type IconName } from "../icons";
import { toneClass, type StatusView, type Tone } from "../status";

interface BaseProps {
  size?: "md" | "lg";
  className?: string;
  /** Hide the icon (rare — only when an adjacent icon already conveys the tone). */
  iconHidden?: boolean;
}

type ViewProps = BaseProps & { view: StatusView; tone?: never; icon?: never; label?: never };
type ExplicitProps = BaseProps & {
  view?: never;
  tone: Tone;
  icon: IconName;
  label: string;
};

export type StatusPillProps = ViewProps | ExplicitProps;

export function StatusPill(props: StatusPillProps) {
  const { size = "md", className = "", iconHidden = false } = props;
  const tone: Tone = "view" in props && props.view ? props.view.tone : props.tone!;
  const icon: IconName = "view" in props && props.view ? props.view.icon : props.icon!;
  const label = "view" in props && props.view ? props.view.label : props.label!;

  const classes = [
    "aac-pill",
    toneClass(tone),
    size === "lg" ? "aac-pill--lg" : "",
    className,
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <span className={classes}>
      {!iconHidden && (
        <Icon name={icon} size={size === "lg" ? 18 : 15} className="aac-pill__icon" />
      )}
      <span>{label}</span>
    </span>
  );
}
