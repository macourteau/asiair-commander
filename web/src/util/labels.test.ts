import { describe, expect, it } from "vitest";
import { unackedAlertsLabel } from "./labels";

describe("unackedAlertsLabel", () => {
  it("pluralises the count", () => {
    expect(unackedAlertsLabel(1)).toBe("1 unacknowledged alert");
    expect(unackedAlertsLabel(2)).toBe("2 unacknowledged alerts");
  });
});
