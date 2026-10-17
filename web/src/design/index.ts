/**
 * Design-system barrel. Wave 2 and future surfaces import components, tokens
 * helpers, icons, and hooks from here:
 *
 *   import { Card, Button, StatusPill, Icon, useToast } from "@/design";
 *
 * The stylesheets (tokens.css, base.css, components.css) are imported once in
 * main.tsx — surfaces never import CSS directly.
 */
export { Button, type ButtonProps, type ButtonVariant } from "./components/Button";
export { IconButton, type IconButtonProps } from "./components/IconButton";
export { Card, type CardProps } from "./components/Card";
export { StatusPill, type StatusPillProps } from "./components/StatusPill";
export { Badge, type BadgeProps } from "./components/Badge";
export {
  Field,
  type FieldProps,
  type FieldControlProps,
} from "./components/Field";
export { TextInput, type TextInputProps } from "./components/TextInput";
export { Select, type SelectProps, type SelectOption } from "./components/Select";
export { Toggle, type ToggleProps } from "./components/Toggle";
export { Dialog, type DialogProps } from "./components/Dialog";
export { Table, type TableProps, type Column } from "./components/Table";
export { Spinner, type SpinnerProps } from "./components/Spinner";
export { EmptyState, type EmptyStateProps } from "./components/EmptyState";
export {
  ToastProvider,
  useToast,
  type ToastKind,
  type ToastOptions,
} from "./components/Toast";

export { Icon, type IconName, type IconProps } from "./icons";
export {
  ThemeProvider,
  useTheme,
  type Theme,
} from "./useTheme";
export { AnnouncerProvider, useAnnounce } from "./announcer";
export {
  toneClass,
  safeToPowerOffView,
  rigStateView,
  hasPendingNote,
  showMonitoringPill,
  frameStateView,
  missingFrameView,
  missingWarningView,
  retargetResultView,
  severityView,
  type Tone,
  type StatusView,
} from "./status";
