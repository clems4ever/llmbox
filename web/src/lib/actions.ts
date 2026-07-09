// perform wraps every mutating API call the dashboard makes so error handling,
// success toasts, and the expired-session bounce live in one place instead of in
// each button handler. Components keep their own loading state and just await it.
import { notifications } from "@mantine/notifications";
import { ApiError } from "../api";
import { redirectToSignIn } from "./navigation";

/** errorMessage renders any thrown value as a user-facing string. */
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export interface PerformOptions {
  /** Success toast to show when the call resolves; omit for silent success. */
  success?: string;
  /** Ran after a successful call — typically a dashboard refresh. */
  onDone?: () => void | Promise<void>;
}

/** perform runs an API action, reporting the outcome through Mantine
 * notifications: a green toast on success (when `success` is given), a red toast
 * on ordinary failure, and a bounce to sign-in on a 401 (an expired session).
 *
 * @arg fn The API call to run.
 * @arg opts Optional success message and post-success callback.
 * @return Promise<boolean> True when the call succeeded, false on any error.
 */
export async function perform(
  fn: () => Promise<unknown>,
  opts: PerformOptions = {},
): Promise<boolean> {
  try {
    await fn();
    if (opts.success) {
      notifications.show({ color: "teal", message: opts.success });
    }
    await opts.onDone?.();
    return true;
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      redirectToSignIn(); // session expired mid-action
      return false;
    }
    notifications.show({
      color: "red",
      title: "Something went wrong",
      message: errorMessage(err),
    });
    return false;
  }
}
