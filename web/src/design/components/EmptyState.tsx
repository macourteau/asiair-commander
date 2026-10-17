/**
 * EmptyState — an empty screen used as an invitation to act, not a dead end
 * (design architecture §9.2 UI-writing). Give it a plain title, a one-line
 * description of what would fill it and why it's empty, and usually an action.
 *
 *   <EmptyState icon="devices" title="No rigs yet"
 *     description="Add your ASIAIR so Commander can start pulling frames each night."
 *     action={<Button variant="primary" onClick={add}>Add a rig</Button>} />
 */
import { Icon, type IconName } from "../icons";
import type { ReactNode } from "react";

export interface EmptyStateProps {
  icon?: IconName;
  title: string;
  description?: ReactNode;
  action?: ReactNode;
}

export function EmptyState({ icon, title, description, action }: EmptyStateProps) {
  return (
    <div className="aac-empty">
      {icon && <Icon name={icon} size={32} className="aac-empty__icon" />}
      <p className="aac-empty__title">{title}</p>
      {description && <p className="aac-empty__desc">{description}</p>}
      {action}
    </div>
  );
}
