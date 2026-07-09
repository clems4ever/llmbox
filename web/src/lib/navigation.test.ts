import { afterEach, describe, expect, it } from "vitest";
import { redirectToSignIn } from "./navigation";

describe("redirectToSignIn", () => {
  const original = window.location;
  afterEach(() => {
    Object.defineProperty(window, "location", { configurable: true, value: original });
  });

  it("points the browser at the sign-in page with a return to /admin", () => {
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { href: "" },
    });
    redirectToSignIn();
    expect(window.location.href).toBe("/signin?return=/admin");
  });
});
