/**
 * Toast — transient notifications for the result of an action ("Rig paused",
 * "Couldn't save — check the highlighted fields"). Mount <ToastProvider> once at
 * the app root; call useToast() anywhere.
 *
 * Accessibility: the toast region is an aria-live area (polite for success/info,
 * assertive+role=alert for errors) so results are announced without stealing
 * focus. Each toast is dismissible and auto-expires (errors linger longer).
 *
 *   const toast = useToast();
 *   toast.success("Rig paused.");
 *   toast.error("Couldn't reach the rig. Check it's powered on and on the network.");
 */
import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Icon, type IconName } from "../icons";
import { IconButton } from "./IconButton";

export type ToastKind = "success" | "error" | "info";

export interface ToastOptions {
  kind?: ToastKind;
  title?: string;
  message: ReactNode;
  /** ms before auto-dismiss; 0 keeps it until dismissed. Defaults by kind. */
  duration?: number;
}

interface ToastItem extends Required<Pick<ToastOptions, "message">> {
  id: number;
  kind: ToastKind;
  title?: string;
}

interface ToastApi {
  show: (opts: ToastOptions) => void;
  success: (message: ReactNode, title?: string) => void;
  error: (message: ReactNode, title?: string) => void;
  info: (message: ReactNode, title?: string) => void;
}

const ToastContext = createContext<ToastApi | null>(null);

const ICONS: Record<ToastKind, IconName> = {
  success: "safe",
  error: "problem",
  info: "info",
};
const DEFAULT_DURATION: Record<ToastKind, number> = {
  success: 4000,
  info: 5000,
  error: 8000,
};

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<ToastItem[]>([]);
  const nextId = useRef(1);

  const dismiss = useCallback((id: number) => {
    setToasts((prev) => prev.filter((t) => t.id !== id));
  }, []);

  const show = useCallback(
    (opts: ToastOptions) => {
      const kind = opts.kind ?? "info";
      const id = nextId.current++;
      setToasts((prev) => [...prev, { id, kind, title: opts.title, message: opts.message }]);
      const duration = opts.duration ?? DEFAULT_DURATION[kind];
      if (duration > 0) {
        window.setTimeout(() => dismiss(id), duration);
      }
    },
    [dismiss],
  );

  const api = useMemo<ToastApi>(
    () => ({
      show,
      success: (message, title) => show({ kind: "success", message, title }),
      error: (message, title) => show({ kind: "error", message, title }),
      info: (message, title) => show({ kind: "info", message, title }),
    }),
    [show],
  );

  return (
    <ToastContext.Provider value={api}>
      {children}
      {/* The polite live region is always mounted, so a toast added to it is
          announced: a role=status element inserted already filled is not read by
          every screen reader. An error is its own role=alert (assertive) inside
          it; success and info carry no role, so nothing is announced twice. */}
      <div className="aac-toaster" aria-live="polite" aria-relevant="additions">
        {toasts.map((t) => (
          <div
            key={t.id}
            className={`aac-toast aac-toast--${t.kind}`}
            role={t.kind === "error" ? "alert" : undefined}
          >
            <Icon name={ICONS[t.kind]} size={18} className="aac-toast__icon" />
            <div className="aac-toast__body">
              {t.title && <div className="aac-toast__title">{t.title}</div>}
              <div>{t.message}</div>
            </div>
            <IconButton
              label="Dismiss notification"
              icon={<Icon name="close" size={16} />}
              onClick={() => dismiss(t.id)}
            />
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

export function useToast(): ToastApi {
  const ctx = useContext(ToastContext);
  if (!ctx) throw new Error("useToast must be used within <ToastProvider>");
  return ctx;
}
