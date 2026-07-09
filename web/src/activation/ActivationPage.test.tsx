import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { ActivationPage } from "./ActivationPage";
import { render } from "../test/utils";
import type { AuthState } from "./api";

/** stubState makes fetch answer the state endpoint with one canned state. */
function stubState(state: AuthState, status = 200): ReturnType<typeof vi.fn> {
  const fn = vi.fn().mockResolvedValue(
    new Response(JSON.stringify(status === 200 ? state : { error: "unknown or expired authentication session" }), {
      status,
      headers: { "Content-Type": "application/json" },
    }),
  );
  vi.stubGlobal("fetch", fn);
  return fn;
}

describe("ActivationPage", () => {
  beforeEach(() => {
    window.history.pushState({}, "", "/auth/tok123");
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("renders the two-step activation flow when the visitor may activate", async () => {
    stubState({
      box_id: "web-box",
      spoke: "edge-1",
      auth_enabled: false,
      logged_in: false,
      status: "pending",
      authorize_url: "https://claude.example/authorize?x=1",
    });
    render(<ActivationPage />);
    expect(await screen.findByText("Activate your llmbox")).toBeInTheDocument();
    expect(screen.getByText("web-box")).toBeInTheDocument();
    expect(screen.getByText("edge-1")).toBeInTheDocument();
    const link = screen.getByRole("link", { name: /Sign in with Claude/ });
    expect(link).toHaveAttribute("href", "https://claude.example/authorize?x=1");
    expect(link).toHaveClass("btn-link");
    expect(document.querySelector("input[name=code]")).toBeInTheDocument();
    expect(document.querySelector("#activate")).toBeInTheDocument();
  });

  it("submits the code and shows the ready view with the session link", async () => {
    const fetchFn = stubState({
      auth_enabled: false,
      logged_in: false,
      status: "pending",
      authorize_url: "https://claude.example/authorize",
    });
    const { user } = render(<ActivationPage />);
    await screen.findByText("Activate your llmbox");

    fetchFn.mockResolvedValueOnce(
      new Response(
        JSON.stringify({ auth_enabled: false, logged_in: false, status: "ready", session_url: "https://claude.ai/code/s/1" }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    await user.type(document.querySelector("input[name=code]") as HTMLInputElement, "THECODE");
    await user.click(document.querySelector("#activate") as HTMLElement);

    expect(await screen.findByText("Your llmbox is ready.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "https://claude.ai/code/s/1" })).toBeInTheDocument();
    // The submit posted the code to the code endpoint.
    const post = fetchFn.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === "POST");
    expect(post?.[0]).toBe("/auth/tok123/code");
    expect(JSON.parse((post?.[1] as RequestInit).body as string)).toEqual({ code: "THECODE", csrf: "" });
  });

  it("shows only the sign-in buttons when not signed in", async () => {
    stubState({
      auth_enabled: true,
      logged_in: false,
      providers: [{ label: "Google", login_path: "/auth/google/login?token=tok123" }],
    });
    render(<ActivationPage />);
    expect(await screen.findByText("Sign in to activate")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Sign in with Google" })).toHaveAttribute(
      "href",
      "/auth/google/login?token=tok123",
    );
    expect(document.querySelector("input[name=code]")).not.toBeInTheDocument();
  });

  it("explains a signed-in non-activator is not authorized", async () => {
    stubState({
      auth_enabled: true,
      logged_in: false,
      not_authorized: true,
      email: "admin@corp.com",
    });
    render(<ActivationPage />);
    expect(await screen.findByText("Not authorized to activate")).toBeInTheDocument();
    expect(screen.getByText(/admin@corp.com/)).toBeInTheDocument();
  });

  it("surfaces an unknown token as an error", async () => {
    stubState({ auth_enabled: false, logged_in: false }, 404);
    render(<ActivationPage />);
    await waitFor(() =>
      expect(screen.getByText("unknown or expired authentication session")).toBeInTheDocument(),
    );
  });
});
