import { describe, expect, it } from "vitest";
import { APP_NAME, pageForPath, pageTitle } from "./pageTitle";

const ROUTES = [
  { to: "/", label: "Dashboard", end: true },
  { to: "/feed", label: "Live feed" },
  { to: "/settings", label: "Settings" },
];

describe("pageForPath", () => {
  it("matches the index route exactly", () => {
    expect(pageForPath("/", ROUTES)).toBe("Dashboard");
    expect(pageForPath("/nope", ROUTES)).toBeUndefined();
  });

  it("matches a section and its sub-paths but not a longer sibling", () => {
    expect(pageForPath("/feed", ROUTES)).toBe("Live feed");
    expect(pageForPath("/feed/", ROUTES)).toBe("Live feed");
    expect(pageForPath("/feed/123", ROUTES)).toBe("Live feed");
    expect(pageForPath("/feedback", ROUTES)).toBeUndefined();
  });
});

describe("pageTitle", () => {
  it("leads with the page and falls back to the app name", () => {
    expect(pageTitle("Settings")).toBe("Settings · ASIAIR Commander");
    expect(pageTitle(undefined)).toBe(APP_NAME);
  });
});
