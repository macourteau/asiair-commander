import { describe, expect, it } from "vitest";
import { themeSwitchView } from "./themeSwitch";

describe("themeSwitchView", () => {
  it("offers dark, with the moon, while the light theme is showing", () => {
    expect(themeSwitchView("light")).toEqual({
      label: "Switch to dark theme",
      icon: "moon",
    });
  });

  it("offers light, with the sun, while the dark theme is showing", () => {
    expect(themeSwitchView("dark")).toEqual({
      label: "Switch to light theme",
      icon: "sun",
    });
  });
});
