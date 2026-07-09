// StatusBadge maps a box phase to a coloured Mantine badge — the single place
// phase colouring is decided, so the table and card views stay consistent.
// StateBadge does the same for the box's runtime state (running / unreachable /
// terminated / backend states like exited).
import { Badge, Tooltip } from "@mantine/core";
import { lastSeenAt, phaseTone, stateTone } from "../lib/format";

const TONE_COLOR = { ready: "teal", pending: "blue", error: "red" } as const;

const STATE_COLOR = {
  running: "teal",
  unreachable: "yellow",
  terminated: "gray",
  stopped: "orange",
} as const;

export interface StatusBadgeProps {
  phase: string;
}

/** StatusBadge renders a box's phase as a coloured pill.
 *
 * @arg props The box phase string.
 * @return JSX.Element The badge.
 */
export function StatusBadge({ phase }: StatusBadgeProps): JSX.Element {
  const tone = phaseTone(phase);
  return (
    <Badge color={TONE_COLOR[tone]} variant="light" radius="sm">
      {phase || "unknown"}
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
