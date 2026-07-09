// Small presentation helpers shared across the dashboard: turning API strings
// into the short, human forms the UI shows. They are pure so they can be unit
// tested directly, keeping formatting logic out of the components.
import type { BoxView } from "../api";

/** shortTime trims an ISO timestamp to "YYYY-MM-DD HH:MM", or "" when empty —
 * the compact form the tables show for enrollment/expiry times.
 *
 * @arg iso An ISO-8601 timestamp, possibly empty or undefined.
 * @return string The "YYYY-MM-DD HH:MM" prefix, or "" when there is no input.
 */
export function shortTime(iso?: string): string {
  if (!iso) return "";
  return iso.slice(0, 16).replace("T", " ");
}

/** isExpired reports whether an ISO expiry timestamp is in the past.
 *
 * @arg iso An ISO-8601 timestamp.
 * @return boolean True when the instant has already elapsed.
 */
export function isExpired(iso: string): boolean {
  return new Date(iso).getTime() < Date.now();
}

/** createdAt renders a box's numeric created field (Unix seconds) as a short
 * local timestamp, or "" when the box carries no creation time.
 *
 * @arg created The box's created field, in Unix seconds (0 when unknown).
 * @return string The "YYYY-MM-DD HH:MM" local time, or "" when created is 0.
 */
export function createdAt(created: number): string {
  if (!created) return "";
  const d = new Date(created * 1000);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** boxId returns the stable identifier the UI uses for a workspace: its box_id
 * when set, else its display name, else its instance ID (a record observed
 * before its first sync may carry no name yet) — the same value the
 * destroy/proxy calls key on.
 *
 * @arg b The box to identify.
 * @return string The box_id, falling back to name, then instance_id.
 */
export function boxId(b: BoxView): string {
  return b.box_id || b.name || b.instance_id;
}

export type PhaseTone = "ready" | "pending" | "error";

/** phaseTone classifies a box phase into a colour tone for its status badge:
 * ready is green, error/failed red, anything else a neutral "pending".
 *
 * @arg phase The box's phase string.
 * @return PhaseTone The tone the badge should use.
 */
export function phaseTone(phase: string): PhaseTone {
  const p = phase.toLowerCase();
  if (p === "ready" || p === "running") return "ready";
  if (p.includes("error") || p.includes("fail")) return "error";
  return "pending";
}

export type StateTone = "running" | "unreachable" | "terminated" | "stopped";

/** stateTone classifies a box state for its badge colour: running is healthy,
 * unreachable means the box's spoke is offline (the box itself may be fine),
 * terminated is a tombstone for a box confirmed gone, and anything else
 * (exited, paused, …) is a stopped-ish backend state.
 *
 * @arg state The box's state string.
 * @return StateTone The tone the state badge should use.
 */
export function stateTone(state: string): StateTone {
  const s = state.toLowerCase();
  if (s === "running") return "running";
  if (s === "unreachable") return "unreachable";
  if (s === "terminated") return "terminated";
  return "stopped";
}

/** lastSeenAt renders a box's last_seen field (Unix seconds) as a short local
 * timestamp, or "" when the hub never observed the box.
 *
 * @arg lastSeen The box's last_seen field, in Unix seconds (0 or undefined when unknown).
 * @return string The "YYYY-MM-DD HH:MM" local time, or "" when unknown.
 */
export function lastSeenAt(lastSeen?: number): string {
  return createdAt(lastSeen ?? 0);
}
