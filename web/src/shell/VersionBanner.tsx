/**
 * VersionBanner — warns when the running UI's build differs from the server's.
 * The SPA is embedded in the binary, so a server upgrade ships a new UI bundle; a
 * tab left open across that upgrade keeps running the OLD bundle while talking to
 * the NEW server. The nav shows the UI's own build (__APP_VERSION__); this banner
 * catches the skew and offers a reload.
 *
 * The server's version arrives over SSE (server_info), re-announced on every
 * reconnect — which a server restart forces — so the skew is caught without
 * polling. Inert whenever either side is "dev"/unset (local and untagged builds).
 */
import { useServerInfo } from "@/api";
import { Icon } from "@/design";
import { updateAvailable } from "../util/version";

export function VersionBanner() {
  const serverInfo = useServerInfo();
  const target = updateAvailable(__APP_VERSION__, serverInfo?.version);
  if (!target) return null;
  return (
    <div className="version-banner" role="status">
      <Icon name="info" size={18} aria-hidden />
      <span className="version-banner__text">
        This page is running an older version ({__APP_VERSION__}). The server has
        updated to {target}. Reload to catch up.
      </span>
      <button
        type="button"
        className="aac-btn aac-btn--primary aac-btn--sm"
        onClick={() => window.location.reload()}
      >
        Reload
      </button>
    </div>
  );
}
