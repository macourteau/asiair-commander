/**
 * Select — a styled native <select> (native keeps keyboard/mobile behavior for
 * free). Pass options as an array or as <option> children.
 *
 *   <Select value={v} onChange={…} options={[{value:"verified", label:"Verified"}]} />
 */
import { forwardRef, type SelectHTMLAttributes } from "react";

export interface SelectOption {
  value: string;
  label: string;
}

export interface SelectProps extends SelectHTMLAttributes<HTMLSelectElement> {
  invalid?: boolean;
  options?: SelectOption[];
}

export const Select = forwardRef<HTMLSelectElement, SelectProps>(function Select(
  { invalid = false, options, className = "", children, ...rest },
  ref,
) {
  const classes = ["aac-select", invalid ? "aac-select--invalid" : "", className]
    .filter(Boolean)
    .join(" ");
  return (
    <select ref={ref} className={classes} {...rest}>
      {options
        ? options.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))
        : children}
    </select>
  );
});
