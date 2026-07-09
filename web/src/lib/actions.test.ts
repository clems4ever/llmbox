import { beforeEach, describe, expect, it, vi } from "vitest";
import { errorMessage, perform } from "./actions";
import { ApiError } from "../api";
import { notifications } from "@mantine/notifications";
import { redirectToSignIn } from "./navigation";

vi.mock("@mantine/notifications", () => ({ notifications: { show: vi.fn() } }));
vi.mock("./navigation", () => ({ redirectToSignIn: vi.fn() }));

const show = vi.mocked(notifications.show);
const redirect = vi.mocked(redirectToSignIn);

beforeEach(() => {
  show.mockReset();
  redirect.mockReset();
});

describe("errorMessage", () => {
  it("uses an Error's message and stringifies non-Errors", () => {
    expect(errorMessage(new Error("nope"))).toBe("nope");
    expect(errorMessage("raw")).toBe("raw");
  });
});

describe("perform", () => {
  it("returns true and shows the success toast when given one", async () => {
    const onDone = vi.fn();
    const ok = await perform(async () => {}, { success: "done", onDone });
    expect(ok).toBe(true);
    expect(show).toHaveBeenCalledWith(expect.objectContaining({ color: "teal", message: "done" }));
    expect(onDone).toHaveBeenCalledOnce();
  });

  it("succeeds silently when no success message is given", async () => {
    const ok = await perform(async () => {});
    expect(ok).toBe(true);
    expect(show).not.toHaveBeenCalled();
  });

  it("shows a red toast and returns false on an ordinary error", async () => {
    const ok = await perform(async () => {
      throw new Error("splat");
    });
    expect(ok).toBe(false);
    expect(show).toHaveBeenCalledWith(expect.objectContaining({ color: "red", message: "splat" }));
    expect(redirect).not.toHaveBeenCalled();
  });

  it("bounces to sign-in on a 401 without a toast", async () => {
    const ok = await perform(async () => {
      throw new ApiError(401, "expired");
    });
    expect(ok).toBe(false);
    expect(redirect).toHaveBeenCalledOnce();
    expect(show).not.toHaveBeenCalled();
  });
});
