/**
 * Login — the shared-password sign-in. Plain language, one field, and a
 * rate-limit-aware error (the backend slows repeated wrong guesses; when it
 * returns Retry-After we tell the user how long to wait rather than showing a
 * code).
 */
import { useState, type FormEvent } from "react";
import { Navigate, useLocation, useNavigate } from "react-router";
import { ApiError } from "@/api";
import { Button, Card, Field, Icon, TextInput } from "@/design";
import { useAuth } from "../auth/AuthContext";

export function Login() {
  const { status, login } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  if (status === "authenticated") return <Navigate to="/" replace />;
  if (status === "setup") return <Navigate to="/setup" replace />;

  const from = (location.state as { from?: string } | null)?.from ?? "/";

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setSubmitting(true);
    try {
      await login(password);
      navigate(from, { replace: true });
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.status === 429) {
          const wait = err.retryAfter
            ? ` Try again in about ${err.retryAfter} seconds.`
            : " Wait a moment before trying again.";
          setError(`Too many attempts.${wait}`);
        } else if (err.status === 401) {
          setError("That password didn't match. Check it and try again.");
        } else {
          setError(err.message);
        }
      } else {
        setError("Couldn't reach the server. Check that Commander is running.");
      }
      setSubmitting(false);
    }
  }

  return (
    <main className="auth-screen">
      <div className="auth-brand">
        <Icon name="rig" size={26} className="auth-brand__mark" />
        <span className="auth-wordmark">
          ASIAIR Commander
          <small>Overnight data shepherd</small>
        </span>
      </div>

      <Card as="form" pad className="auth-card" onSubmit={onSubmit}>
        <h1 className="auth-title">Sign in</h1>
        <p className="auth-lede">Enter the password you set for this Commander.</p>

        {error && (
          <p className="auth-formerror" role="alert" style={{ marginBottom: "var(--space-4)" }}>
            <Icon name="problem" size={18} />
            <span>{error}</span>
          </p>
        )}

        <div className="stack" style={{ ["--stack-gap" as string]: "var(--space-4)" }}>
          <Field label="Password">
            {(f) => (
              <TextInput
                {...f}
                type="password"
                autoComplete="current-password"
                autoFocus
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="Your Commander password"
              />
            )}
          </Field>
          <Button type="submit" variant="primary" block loading={submitting}>
            Sign in
          </Button>
        </div>
      </Card>
    </main>
  );
}
