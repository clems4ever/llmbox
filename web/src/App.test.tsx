import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { App } from "./App";
import { ApiError } from "./api";
import * as api from "./api";
import { redirectToSignIn } from "./lib/navigation";
import { render } from "./test/utils";

vi.mock("./lib/navigation", () => ({ redirectToSignIn: vi.fn() }));

const meMock = vi.spyOn(api, "me");
const redirect = vi.mocked(redirectToSignIn);

beforeEach(() => {
  redirect.mockReset();
  // Dashboard's initial refresh hits fetch; return empty bodies so it loads clean.
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) } as Response),
  );
});
afterEach(() => vi.unstubAllGlobals());

describe("App boot", () => {
  it("shows the loader while the session is resolving", () => {
    meMock.mockReturnValue(new Promise(() => {}));
    render(<App />);
    expect(screen.getByText("Loading…")).toBeInTheDocument();
  });

  it("renders the dashboard for an admin session", async () => {
    meMock.mockResolvedValue({ email: "a@b.c", admin: true, csrf: "x" });
    render(<App />);
    // "Workspaces" is both the nav item and the view heading.
    expect((await screen.findAllByText("Workspaces")).length).toBeGreaterThan(0);
    expect(screen.getByText("Infrastructure")).toBeInTheDocument();
  });

  it("shows the not-admin notice for a non-admin session", async () => {
    meMock.mockResolvedValue({ email: "user@b.c", admin: false, csrf: "x" });
    render(<App />);
    expect(await screen.findByText("Administrator access required")).toBeInTheDocument();
    expect(screen.getAllByText(/user@b\.c/).length).toBeGreaterThan(0);
  });

  it("bounces to sign-in on a 401", async () => {
    meMock.mockRejectedValue(new ApiError(401, "no session"));
    render(<App />);
    await waitFor(() => expect(redirect).toHaveBeenCalledOnce());
    expect(screen.getByText("Redirecting to sign in…")).toBeInTheDocument();
  });

  it("shows an error alert on a non-401 failure", async () => {
    meMock.mockRejectedValue(new Error("network down"));
    render(<App />);
    expect(await screen.findByText("Couldn't load the dashboard")).toBeInTheDocument();
    expect(screen.getByText("network down")).toBeInTheDocument();
  });
});
