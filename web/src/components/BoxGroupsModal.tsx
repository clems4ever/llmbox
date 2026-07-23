// BoxGroupsModal edits the allowlist groups assigned to one workspace. Global
// groups always apply and are shown checked-and-disabled; the operator toggles
// the extra (non-global) groups this workspace may additionally reach. It is used
// both from the Assignments tab and the workspace details drawer.
import { useEffect, useState } from "react";
import { Alert, Button, Checkbox, Group, Loader, Modal, Stack, Text } from "@mantine/core";
import { IconCheck, IconInfoCircle } from "@tabler/icons-react";
import type { Api, AllowlistGroup, BoxView } from "../api";
import { perform } from "../lib/actions";
import { boxId } from "../lib/format";

export interface BoxGroupsModalProps {
  api: Api;
  /** The workspace whose groups are edited, or null when closed. */
  box: BoxView | null;
  groups: AllowlistGroup[];
  opened: boolean;
  onClose: () => void;
  onSaved: () => Promise<void>;
}

/** BoxGroupsModal renders the per-workspace group assignment form.
 *
 * @arg props The api client, the workspace, all groups, open state, and callbacks.
 * @return JSX.Element The modal.
 */
export function BoxGroupsModal({ api, box, groups, opened, onClose, onSaved }: BoxGroupsModalProps): JSX.Element {
  const [selected, setSelected] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    let cancelled = false;
    if (!opened || !box) return;
    setLoading(true);
    void (async () => {
      try {
        const al = await api.getBoxAllowlist(boxId(box));
        if (!cancelled) setSelected(al.group_ids);
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [opened, box, api]);

  const nonGlobal = groups.filter((g) => !g.is_global);
  const globals = groups.filter((g) => g.is_global);

  const toggle = (id: string, on: boolean) =>
    setSelected((cur) => (on ? [...cur, id] : cur.filter((x) => x !== id)));

  const submit = async () => {
    if (!box) return;
    setSubmitting(true);
    const ok = await perform(() => api.setBoxGroups(boxId(box), selected), {
      success: `updated groups for ${boxId(box)}`,
      onDone: onSaved,
    });
    setSubmitting(false);
    if (ok) onClose();
  };

  return (
    <Modal opened={opened} onClose={onClose} title={box ? `Groups · ${boxId(box)}` : "Groups"} centered>
      {loading ? (
        <Group justify="center" p="lg">
          <Loader size="sm" />
        </Group>
      ) : (
        <Stack gap="sm">
          <Alert variant="light" icon={<IconInfoCircle size={16} />}>
            Global groups apply to every workspace and can't be removed here. Toggle the extra groups this
            workspace may additionally reach.
          </Alert>
          {globals.map((g) => (
            <Checkbox key={g.id} checked disabled label={`${g.name} (global)`} description={g.description || undefined} />
          ))}
          {nonGlobal.length === 0 ? (
            <Text c="dimmed" size="sm">
              No optional groups to assign — every group is global.
            </Text>
          ) : (
            nonGlobal.map((g) => (
              <Checkbox
                key={g.id}
                checked={selected.includes(g.id)}
                onChange={(e) => toggle(g.id, e.currentTarget.checked)}
                label={g.name}
                description={g.description || `${g.domains.length} domains`}
              />
            ))
          )}
          <Group justify="flex-end" mt="xs">
            <Button variant="subtle" onClick={onClose}>
              Cancel
            </Button>
            <Button onClick={() => void submit()} loading={submitting} leftSection={<IconCheck size={16} />}>
              Save groups
            </Button>
          </Group>
        </Stack>
      )}
    </Modal>
  );
}
