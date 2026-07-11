// StatusBadge renders a broken box's phase as a red Mantine badge — a box whose
// init script failed. A healthy box has no phase to show, so the badge renders
// nothing. StateBadge does the same for the box's runtime state (running /
// unreachable / terminated / backend states like exited).
import { Badge, Tooltip } from "@mantine/core";
import { isBroken, lastSeenAt, stateTone } from "../lib/format";

const STATE_COLOR = {
  running: "teal",
  unreachable: "yellow",
  terminated: "gray",
  paused: "grape",
  stopped: "orange",
} as const;

export interface StatusBadgeProps {
  phase?: string;
}

/** StatusBadge renders a red "broken" pill for a broken box, or nothing for a
 * healthy one (whose phase is empty).
 *
 * @arg props The box phase string.
 * @return JSX.Element | null The badge, or null when the box is not broken.
 */
export function StatusBadge({ phase }: StatusBadgeProps): JSX.Element | null {
  if (!isBroken(phase)) return null;
  return (
    <Badge color="red" variant="light" radius="sm">
      broken
    </Badge>
  );
}

export interface StateBadgeProps {
  state: string;
  lastSeen?: number;
}

/** StateBadge renders a box's runtime state as a coloured pill. An unreachable
 * or terminated box gets a tooltip explaining what the state means and, when
 * known, when the hub last observed the box on its spoke.
 *
 * @arg props The box state string and optional last-seen Unix timestamp.
 * @return JSX.Element The badge.
 */
export function StateBadge({ state, lastSeen }: StateBadgeProps): JSX.Element {
  const tone = stateTone(state);
  const badge = (
    <Badge color={STATE_COLOR[tone]} variant="light" radius="sm" data-box-state={state}>
      {state || "unknown"}
    </Badge>
  );
  if (tone !== "unreachable" && tone !== "terminated") return badge;
  const seen = lastSeenAt(lastSeen);
  const explain =
    tone === "unreachable"
      ? "This workspace's runner is offline; the workspace may still be running."
      : "This workspace no longer exists on its runner.";
  return <Tooltip label={seen ? `${explain} Last seen ${seen}.` : explain}>{badge}</Tooltip>;
}
