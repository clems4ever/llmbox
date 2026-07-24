import { afterEach, describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import { SignInPage } from "./SignInPage";
import { render } from "../test/utils";

/** stubState makes fetch answer /signin/state with one canned state. */
function stubState(state: unknown): void {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(
      new Response(JSON.stringify(state), { status: 200, headers: { "Content-Type": "application/json" } }),
    ),
  );
}

describe("SignInPage", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("lists the provider buttons returning to the target", async () => {
    stubState({
      signed_in: false,
      return_to: "/dash",
      providers: [{ label: "Google", login_path: "/auth/google/login?return=%2Fdash" }],
    });
    render(<SignInPage />);
    expect(await screen.findByRole("link", { name: "Sign in with Google" })).toHaveAttribute(
      "href",
      "/auth/google/login?return=%2Fdash",
    );
  });

  it("renders a button per configured provider", async () => {
    stubState({
      signed_in: false,
      return_to: "/dash",
      providers: [
        { label: "Google", login_path: "/auth/google/login?return=%2Fdash" },
        { label: "GitHub", login_path: "/auth/github/login?return=%2Fdash" },
      ],
    });
    render(<SignInPage />);
    expect(await screen.findByRole("link", { name: "Sign in with Google" })).toHaveAttribute(
      "href",
      "/auth/google/login?return=%2Fdash",
    );
    expect(screen.getByRole("link", { name: "Sign in with GitHub" })).toHaveAttribute(
      "href",
      "/auth/github/login?return=%2Fdash",
    );
  });

  it("shows the no-destination notice when the return target is unsafe", async () => {
    stubState({ signed_in: false });
    render(<SignInPage />);
    expect(
      await screen.findByText(/No valid destination to sign in for/),
    ).toBeInTheDocument();
  });
});
