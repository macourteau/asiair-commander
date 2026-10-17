/**
 * Dialog / Modal — an accessible modal rendered in a portal.
 *
 * Accessibility: role="dialog" + aria-modal, labelled by its title; focus moves
 * in on open and is TRAPPED (Tab/Shift+Tab cycle within); Esc closes; clicking
 * the scrim closes; on close, focus returns to the element that opened it. Body
 * scroll is locked while open.
 *
 *   <Dialog open={open} onClose={close} title="Remove rig?"
 *           footer={<><Button onClick={close}>Cancel</Button>
 *                     <Button variant="danger" onClick={confirm}>Remove</Button></>}>
 *     <p>This stops managing "Main Rig". Files already on the NAS are untouched.</p>
 *   </Dialog>
 */
import {
  useCallback,
  useEffect,
  useId,
  useRef,
  type ReactNode,
  type Ref,
  type RefObject,
} from "react";
import { createPortal } from "react-dom";
import { Icon } from "../icons";
import { IconButton } from "./IconButton";

export interface DialogProps {
  open: boolean;
  onClose: () => void;
  title: string;
  children: ReactNode;
  footer?: ReactNode;
  /** Optional wider dialog for content-heavy flows. */
  size?: "md" | "lg";
  /** Whether a click on the scrim closes (default true). A flow whose content
   * exists nowhere else turns it off so a stray tap cannot lose it; Esc and the
   * header X still call onClose. */
  dismissOnScrim?: boolean;
  /** The title heading, focusable programmatically, for a flow that moves focus
   * to it when its content changes. */
  titleRef?: Ref<HTMLHeadingElement>;
  /** The element focused on open in place of the first focusable (the header's
   * Close button). The dialog's own effect runs after its body's, so a body
   * that focused a field itself would lose it to the Close button. */
  initialFocus?: RefObject<HTMLElement | null>;
}

const FOCUSABLE =
  'a[href],button:not([disabled]),textarea:not([disabled]),input:not([disabled]),select:not([disabled]),[tabindex]:not([tabindex="-1"])';

export function Dialog({
  open,
  onClose,
  title,
  children,
  footer,
  size = "md",
  dismissOnScrim = true,
  titleRef,
  initialFocus,
}: DialogProps) {
  const dialogRef = useRef<HTMLDivElement>(null);
  const restoreFocus = useRef<HTMLElement | null>(null);
  const titleId = useId();

  // Consumers pass an inline arrow, so onClose has a fresh identity on every
  // parent render — and a surface like History re-renders whenever a live frame
  // event lands. Read it through a ref so it can't be an effect dependency: a
  // re-run would re-focus the header, and the dialog element is its own scroll
  // container, so that drags a mid-scroll reader back up to the top.
  const closeRef = useRef(onClose);
  useEffect(() => {
    closeRef.current = onClose;
  }, [onClose]);

  const focusables = useCallback((): HTMLElement[] => {
    const root = dialogRef.current;
    if (!root) return [];
    return Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
      (el) => el.offsetParent !== null || el === document.activeElement,
    );
  }, []);

  useEffect(() => {
    if (!open) return;
    restoreFocus.current = document.activeElement as HTMLElement | null;

    // Move focus into the dialog: the requested element, else the first
    // focusable, else the dialog itself. The opener is recorded first.
    const first = initialFocus?.current ?? focusables()[0] ?? dialogRef.current;
    first?.focus();

    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";

    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        closeRef.current();
        return;
      }
      if (e.key !== "Tab") return;
      const items = focusables();
      if (items.length === 0) {
        e.preventDefault();
        return;
      }
      const first = items[0]!;
      const last = items[items.length - 1]!;
      const active = document.activeElement as HTMLElement;
      if (e.shiftKey && (active === first || !dialogRef.current?.contains(active))) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && active === last) {
        e.preventDefault();
        first.focus();
      }
    };

    document.addEventListener("keydown", onKeyDown, true);
    return () => {
      document.removeEventListener("keydown", onKeyDown, true);
      document.body.style.overflow = prevOverflow;
      restoreFocus.current?.focus?.();
    };
    // initialFocus is read once, on open, like the opener.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, focusables]);

  if (!open) return null;

  return createPortal(
    <div
      className="aac-scrim"
      onMouseDown={(e) => {
        if (dismissOnScrim && e.target === e.currentTarget) onClose();
      }}
    >
      <div
        ref={dialogRef}
        className="aac-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        style={size === "lg" ? { maxWidth: 720 } : undefined}
      >
        <header className="aac-dialog__header">
          <h2 className="aac-dialog__title" id={titleId} ref={titleRef} tabIndex={-1}>
            {title}
          </h2>
          <IconButton label="Close" icon={<Icon name="close" />} onClick={onClose} />
        </header>
        <div className="aac-dialog__body">{children}</div>
        {footer && <footer className="aac-dialog__footer">{footer}</footer>}
      </div>
    </div>,
    document.body,
  );
}
