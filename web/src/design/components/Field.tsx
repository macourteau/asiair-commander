/**
 * Field / FormRow — the labelled form wrapper that enforces the brief's copy
 * rules: every non-obvious control gets a plain-language help line AND, where it
 * clarifies faster than prose, a concrete example (e.g. "'NGC 7000' becomes
 * 'NGC7000'"). The example renders behind an "e.g." prefix, because a bare value
 * sitting above the input reads as the field's current value. A line that is not
 * an example but a live readout of the field (a unit conversion, facts about the
 * bound device) goes in `note`, which renders the same way without the prefix.
 * The label, help, example, note, and error are wired to the control via
 * aria-describedby so a screen reader reads them with the field.
 *
 * Use the `useField` helper to get the ids to spread onto your input, OR use the
 * simpler pattern: pass a render function that receives {id, describedBy, invalid}.
 *
 *   <Field label="Target name" help="…" example="'NGC 7000' → 'NGC7000'" error={err}>
 *     {(f) => <TextInput {...f} value={v} onChange={…} />}
 *   </Field>
 */
import { useId, type ReactNode } from "react";
import { Icon } from "../icons";

export interface FieldControlProps {
  id: string;
  "aria-describedby": string | undefined;
  "aria-invalid": boolean | undefined;
}

export interface FieldProps {
  label: ReactNode;
  /** Plain-language guidance shown under the label. */
  help?: ReactNode;
  /** A concrete example that demonstrates the control (rendered in mono, dimmed,
   * after an "e.g." prefix the component adds — pass the bare example). */
  example?: ReactNode;
  /** A live readout about the field's value (rendered like an example, with no prefix). */
  note?: ReactNode;
  /** Actionable error — what happened and what to do. Presence marks the field invalid. */
  error?: ReactNode;
  required?: boolean;
  /** Render-prop receiving the wiring props to spread onto the control. */
  children: (control: FieldControlProps) => ReactNode;
}

export function Field({
  label,
  help,
  example,
  note,
  error,
  required = false,
  children,
}: FieldProps) {
  const base = useId();
  const helpId = help ? `${base}-help` : undefined;
  const exampleId = example ? `${base}-ex` : undefined;
  const noteId = note ? `${base}-note` : undefined;
  const errorId = error ? `${base}-err` : undefined;
  const describedBy =
    [helpId, exampleId, noteId, errorId].filter(Boolean).join(" ") || undefined;

  return (
    <div className="aac-field">
      <label className="aac-field__label" htmlFor={base}>
        {label}
        {required && (
          <span className="aac-field__req" aria-hidden="true">
            *
          </span>
        )}
      </label>
      {help && (
        <p className="aac-field__help" id={helpId}>
          {help}
        </p>
      )}
      {example && (
        <p className="aac-field__example" id={exampleId}>
          e.g.{" "}
          <span className="mono">{example}</span>
        </p>
      )}
      {note && (
        <p className="aac-field__example mono" id={noteId}>
          {note}
        </p>
      )}
      {children({
        id: base,
        "aria-describedby": describedBy,
        "aria-invalid": error ? true : undefined,
      })}
      {error && (
        <p className="aac-field__error" id={errorId} role="alert">
          <Icon name="problem" size={14} />
          <span>{error}</span>
        </p>
      )}
    </div>
  );
}
