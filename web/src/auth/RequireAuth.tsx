/**
 * Route guard for the authenticated app. Redirects to Setup or Login as the auth
 * status dictates, and shows a full-page spinner while the bootstrap probe runs.
 */
import { Navigate, Outlet, useLocation } from "react-router";
import { Spinner } from "@/design";
import { useAuth } from "./AuthContext";

export function RequireAuth() {
  const { status } = useAuth();
  const location = useLocation();

  if (status === "loading") {
    return (
      <div
        style={{
          minHeight: "100vh",
          display: "grid",
          placeItems: "center",
        }}
      >
        <Spinner label="Loading…" />
      </div>
    );
  }
  if (status === "setup") return <Navigate to="/setup" replace />;
  if (status === "unauthenticated") {
    return <Navigate to="/login" replace state={{ from: location.pathname }} />;
  }
  return <Outlet />;
}
