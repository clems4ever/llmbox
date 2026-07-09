// confirmDestroy is the one destructive-action gate the dashboard uses: it opens
// a red confirm modal and, on confirm, runs the action through perform() so the
// success toast, error handling, and 401 bounce are consistent everywhere. Used
// by the remove-workspace, drop-spoke, revoke-token, and delete-proxy buttons.
import { Text } from "@mantine/core";
import { modals } from "@mantine/modals";
import { perform } from "./actions";

export interface ConfirmDestroyOptions {
  title: string;
  message: string;
  confirmLabel?: string;
  action: () => Promise<unknown>;
  success?: string;
  refresh: () => Promise<void>;
}

/** confirmDestroy opens a confirmation modal and performs the action on confirm.
 *
 * @arg opts The modal copy, the action to run, and the refresh to follow it.
 */
export function confirmDestroy(opts: ConfirmDestroyOptions): void {
  modals.openConfirmModal({
    title: opts.title,
    children: <Text size="sm">{opts.message}</Text>,
    labels: { confirm: opts.confirmLabel ?? "Remove", cancel: "Cancel" },
    confirmProps: { color: "red" },
    onConfirm: () => {
      void perform(opts.action, { success: opts.success, onDone: opts.refresh });
    },
  });
}
