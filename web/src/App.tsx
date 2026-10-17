/**
 * App — routing and the authenticated/unauthenticated split.
 *
 *   /login, /setup          — unauthenticated surfaces (self-redirect when signed in)
 *   everything else          — guarded by <RequireAuth>, laid out in <AppShell>
 *
 * Uses BrowserRouter; the Go backend's SPA-history fallback serves index.html for
 * any non-/api, non-/events path so deep links and reloads work.
 */
import { createBrowserRouter, Navigate, useRouteError } from "react-router";
import { RouterProvider } from "react-router/dom";
import { Button, Card, Icon } from "@/design";
import { AppShell } from "./shell/AppShell";
import { RequireAuth } from "./auth/RequireAuth";
import { Login } from "./surfaces/Login";
import { Setup } from "./surfaces/Setup";
import { Dashboard } from "./surfaces/Dashboard";
import { Devices } from "./surfaces/Devices";
import { Feed } from "./surfaces/Feed";
import { History } from "./surfaces/History";
import { Tools } from "./surfaces/Tools";
import { DeviceFiles } from "./surfaces/DeviceFiles";
import { Settings } from "./surfaces/Settings";
import { Logs } from "./surfaces/Logs";
import { Alerts } from "./surfaces/Alerts";

// A data router (createBrowserRouter) rather than <BrowserRouter> so surfaces can
// use useBlocker to guard in-app navigation away from unsaved edits (Settings).
// A pathless root route carries an errorElement so an unexpected throw in any
// surface renders a contained boundary instead of blanking the whole app.
const router = createBrowserRouter([
  {
    errorElement: <RouteError />,
    children: [
      { path: "/login", element: <Login /> },
      { path: "/setup", element: <Setup /> },
      {
        element: <RequireAuth />,
        children: [
          {
            element: <AppShell />,
            children: [
              { index: true, element: <Dashboard /> },
              { path: "feed", element: <Feed /> },
              { path: "devices", element: <Devices /> },
              { path: "history", element: <History /> },
              { path: "tools", element: <Tools /> },
              { path: "files", element: <DeviceFiles /> },
              { path: "settings", element: <Settings /> },
              { path: "logs", element: <Logs /> },
              { path: "alerts", element: <Alerts /> },
            ],
          },
        ],
      },
      { path: "*", element: <Navigate to="/" replace /> },
    ],
  },
]);

export function App() {
  return <RouterProvider router={router} />;
}

/** Last-resort boundary for an unhandled render/loader error in any surface. */
function RouteError() {
  const error = useRouteError();
  const detail =
    error instanceof Error
      ? error.message
      : typeof error === "string"
        ? error
        : "";
  return (
    <div style={{ display: "grid", placeItems: "center", minHeight: "100vh", padding: "var(--space-4)" }}>
      <Card pad style={{ maxWidth: 460 }}>
        <p className="notice tone-problem" role="alert">
          <Icon name="problem" size={18} />
          <span>Something went wrong displaying this page.</span>
        </p>
        {detail && <p className="text-sm text-muted break-anywhere">{detail}</p>}
        <div style={{ marginTop: "var(--space-3)" }}>
          <Button variant="primary" onClick={() => window.location.assign("/")}>
            Reload Commander
          </Button>
        </div>
      </Card>
    </div>
  );
}
