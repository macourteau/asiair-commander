/**
 * AllowedHostsField — the editor for the addresses Commander answers to, shared
 * by the first-run wizard and Settings so the two cannot drift. It owns the rows,
 * add/remove, the IP-literal warning, and the rendering of per-entry validation
 * problems returned by the server.
 *
 * The list is a security control worded as an ordinary setting: an address that is
 * not listed is refused, which is what stops a page on another site from driving
 * Commander through the operator's browser. The copy therefore says what the list
 * does, not what a Host header is.
 */
import { useId, type ReactNode } from "react";
import type { Problem } from "@/api";
import { Button, Icon, IconButton, TextInput } from "@/design";
import "./allowed-hosts.css";

export interface AllowedHostsFieldProps {
  /** One address per row. An empty array renders a single empty row to type into. */
  value: string[];
  onChange: (hosts: string[]) => void;
  /** Locked once the addresses have been committed and a retry cannot resend them. */
  disabled?: boolean;
  label?: string;
  help?: ReactNode;
  /**
   * Validation findings for `web.allowed_hosts` from a rejected save. Each is
   * attached to the row it names, so the correction lands where the operator must
   * act; anything naming an address that is not in the list (the lockout guard's
   * "keep X in the list") is shown against the whole list instead.
   */
  problems?: Problem[];
}

/** Whether an entry is a bare IP literal rather than a name. */
function isIPLiteral(host: string): boolean {
  // Mirror the server's NormalizeHostEntry: unwrap a bracketed IPv6 literal and
  // drop a ":port" suffix, the latter only when what remains has no colon of its
  // own so a bare "::1" is not read as host:port.
  let h = host.trim();
  const bracketed = /^\[(.+)\](?::\d+)?$/.exec(h);
  h = bracketed?.[1] ?? /^([^:]+):\d+$/.exec(h)?.[1] ?? h;
  if (!h) return false;
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(h)) return true;
  // IPv6 is bracketed in a URL but stored bare. Two or more colons, because a
  // single one is a ":port" suffix on a name ("nas.local:8686") — treating that
  // as an IP would warn about DHCP churn on an entry that has no address in it.
  return (h.match(/:/g)?.length ?? 0) >= 2;
}

/** The address a problem message names, taken from its first quoted run. */
function problemSubject(message: string): string {
  return message.match(/"([^"]*)"/)?.[1]?.trim().toLowerCase() ?? "";
}

export function AllowedHostsField({
  value,
  onChange,
  disabled = false,
  label = "Addresses you'll use to reach Commander",
  help,
  problems = [],
}: AllowedHostsFieldProps) {
  const base = useId();
  const helpId = `${base}-help`;
  // Always show something to type into, without writing a phantom entry back to
  // the caller — an empty row is a UI affordance, not a value.
  const rows = value.length > 0 ? value : [""];

  const set = (i: number, host: string) =>
    onChange(rows.map((h, idx) => (idx === i ? host : h)));
  const remove = (i: number) => onChange(rows.filter((_, idx) => idx !== i));
  const add = () => onChange([...rows, ""]);

  const errorFor = (host: string): Problem | undefined => {
    const key = host.trim().toLowerCase();
    if (!key) return undefined;
    return problems.find((p) => problemSubject(p.Message) === key);
  };
  const listed = new Set(rows.map((h) => h.trim().toLowerCase()));
  const unattached = problems.filter((p) => !listed.has(problemSubject(p.Message)));

  const ipEntry = rows.find((h) => isIPLiteral(h));

  return (
    <div className="aac-field">
      <span className="aac-field__label" id={`${base}-label`}>
        {label}
      </span>
      {help && (
        <p className="aac-field__help" id={helpId}>
          {help}
        </p>
      )}

      <div className="allowed-hosts" role="group" aria-labelledby={`${base}-label`}>
        {rows.map((host, i) => {
          const problem = errorFor(host);
          return (
            <div className="allowed-hosts__entry" key={i}>
              <div className="allowed-hosts__row">
                <TextInput
                  mono
                  aria-label={`Address ${i + 1}`}
                  aria-describedby={help ? helpId : undefined}
                  invalid={problem?.Severity === "error"}
                  placeholder="nas.local"
                  value={host}
                  disabled={disabled}
                  onChange={(e) => set(i, e.target.value)}
                />
                <IconButton
                  label={`Remove address ${i + 1}`}
                  icon={<Icon name="trash" size={16} />}
                  disabled={disabled || rows.length <= 1}
                  onClick={() => remove(i)}
                />
              </div>
              {problem && (
                <p
                  className={
                    problem.Severity === "error"
                      ? "aac-field__error"
                      : "allowed-hosts__warning"
                  }
                  role={problem.Severity === "error" ? "alert" : undefined}
                >
                  <Icon
                    name={problem.Severity === "error" ? "problem" : "alert"}
                    size={14}
                  />
                  <span>{problem.Message}</span>
                </p>
              )}
            </div>
          );
        })}
      </div>

      {ipEntry && !problems.some((p) => problemSubject(p.Message) === ipEntry.trim().toLowerCase()) && (
        <p className="allowed-hosts__warning">
          <Icon name="alert" size={14} />
          <span>
            <code>{ipEntry.trim()}</code> is an IP address, which can change when your
            router hands out a new lease. Add a hostname as well so you don't lose
            access.
          </span>
        </p>
      )}

      {unattached.map((p, i) => (
        <p
          key={i}
          className={
            p.Severity === "error" ? "aac-field__error" : "allowed-hosts__warning"
          }
          role={p.Severity === "error" ? "alert" : undefined}
        >
          <Icon name={p.Severity === "error" ? "problem" : "alert"} size={14} />
          <span>{p.Message}</span>
        </p>
      ))}

      {!disabled && (
        <div className="allowed-hosts__add">
          <Button size="sm" icon={<Icon name="add" size={16} />} onClick={add}>
            Add address
          </Button>
        </div>
      )}
    </div>
  );
}
