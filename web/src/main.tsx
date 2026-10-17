import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

// Design system stylesheets — loaded once, in cascade order: tokens → base →
// components. App-level layout (shell, auth) follows. Surface-specific CSS is
// co-located with its surface (e.g. dashboard.css).
import "./design/tokens.css";
import "./design/base.css";
import "./design/components.css";
import "./shell/shell.css";
import "./auth/auth.css";

import { ThemeProvider, AnnouncerProvider, ToastProvider } from "@/design";
import { AuthProvider } from "./auth/AuthContext";
import { App } from "./App";
import { captureSetupToken } from "./auth/setupToken";

// Read the one-time setup token out of the URL fragment BEFORE anything mounts,
// and strip it from the address bar. This cannot wait for the Setup surface: that
// renders only after the auth bootstrap resolves to "setup", and any redirect on
// the way there (a transient probe failure landing on /login) drops the hash and
// destroys the token with no way to recover it short of restarting the service.
captureSetupToken();

const rootEl = document.getElementById("root");
if (!rootEl) throw new Error("Root element #root not found");

createRoot(rootEl).render(
  <StrictMode>
    <ThemeProvider>
      <AnnouncerProvider>
        <ToastProvider>
          <AuthProvider>
            <App />
          </AuthProvider>
        </ToastProvider>
      </AnnouncerProvider>
    </ThemeProvider>
  </StrictMode>,
);
