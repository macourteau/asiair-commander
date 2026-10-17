/**
 * TextInput — a styled <input>. Set `mono` for machine data (paths, hosts,
 * templates). `invalid` draws the error border; pair with a <Field error> so the
 * message is announced. Forwards all native input props (type, value, onChange…).
 */
import { forwardRef, type InputHTMLAttributes } from "react";

export interface TextInputProps extends InputHTMLAttributes<HTMLInputElement> {
  invalid?: boolean;
  mono?: boolean;
}

export const TextInput = forwardRef<HTMLInputElement, TextInputProps>(
  function TextInput({ invalid = false, mono = false, className = "", ...rest }, ref) {
    const classes = [
      "aac-input",
      invalid ? "aac-input--invalid" : "",
      mono ? "aac-input--mono" : "",
      className,
    ]
      .filter(Boolean)
      .join(" ");
    return <input ref={ref} className={classes} {...rest} />;
  },
);
