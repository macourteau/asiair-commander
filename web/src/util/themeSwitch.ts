/**
 * The nav's theme control: a button that names the action it performs. A switch
 * labelled with the current theme leaves unclear which position means which
 * theme; "Switch to dark theme" cannot be misread. The icon is the target
 * theme's, matching the label.
 */
import type { IconName, Theme } from "@/design";

export interface ThemeSwitchView {
  /** The button's visible text, which is also its accessible name. */
  label: string;
  icon: IconName;
}

export function themeSwitchView(current: Theme): ThemeSwitchView {
  return current === "light"
    ? { label: "Switch to dark theme", icon: "moon" }
    : { label: "Switch to light theme", icon: "sun" };
}
