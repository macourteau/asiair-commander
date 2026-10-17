/**
 * Toggle — an on/off switch backed by a real checkbox (keyboard + form semantics
 * for free). The visible label is required and clickable. State is conveyed by
 * position AND the label, not color alone.
 *
 *   <Toggle checked={managed} onChange={setManaged} label="Managed rig" />
 */
import { useId } from "react";

export interface ToggleProps {
  checked: boolean;
  onChange: (checked: boolean) => void;
  label: string;
  disabled?: boolean;
  /** Extra description id(s) wired via aria-describedby (e.g. a Field help line). */
  "aria-describedby"?: string;
  id?: string;
}

export function Toggle({
  checked,
  onChange,
  label,
  disabled = false,
  id,
  ...aria
}: ToggleProps) {
  const auto = useId();
  const inputId = id ?? auto;
  return (
    <label className="aac-toggle" htmlFor={inputId}>
      <input
        id={inputId}
        type="checkbox"
        role="switch"
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
        aria-describedby={aria["aria-describedby"]}
      />
      <span className="aac-toggle__track" aria-hidden="true">
        <span className="aac-toggle__thumb" />
      </span>
      <span className="aac-toggle__label">{label}</span>
    </label>
  );
}
