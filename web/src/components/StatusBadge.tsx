// StatusBadge maps a box phase to a coloured Mantine badge — the single place
// phase colouring is decided, so the table and card views stay consistent.
import { Badge } from "@mantine/core";
import { phaseTone } from "../lib/format";

const TONE_COLOR = { ready: "teal", pending: "blue", error: "red" } as const;

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
