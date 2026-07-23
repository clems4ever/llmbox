// AddToGroupModal is the one-click "allow this domain" flow from the DNS audit
// view: it adds a domain (usually one just blocked) to an existing allowlist
// group, or creates a new group holding it. Saving re-pushes policy so the box
// can reach the domain right away.
import { useEffect, useState } from "react";
import { Alert, Button, Group, Modal, NativeSelect, Stack, Text, TextInput } from "@mantine/core";
import { IconCheck, IconWorld } from "@tabler/icons-react";
import type { AllowlistGroup, Api } from "../api";
import { perform } from "../lib/actions";

export interface AddToGroupModalProps {
  api: Api;
  /** The audited domain to allow, or null when the modal is closed. */
  domain: string | null;
  groups: AllowlistGroup[];
  opened: boolean;
  onClose: () => void;
  onDone: () => Promise<void>;
}

const NEW = "__new__";

/** AddToGroupModal renders the add-a-domain-to-a-group form.
 *
 * @arg props The api client, the domain, all groups, open state, and callbacks.
 * @return JSX.Element The modal.
 */
export function AddToGroupModal({ api, domain, groups, opened, onClose, onDone }: AddToGroupModalProps): JSX.Element {
  const [target, setTarget] = useState<string>(NEW);
  const [newName, setNewName] = useState("");
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (opened) {
      setTarget(groups[0]?.id ?? NEW);
      setNewName("");
    }
  }, [opened, groups]);

  const submit = async () => {
    if (!domain) return;
    setSubmitting(true);
    const ok = await perform(
      () =>
        api.addDomainToGroup(
          domain,
          target === NEW ? { newGroupName: newName.trim() } : { groupId: target },
        ),
      { success: `allowed ${domain}`, onDone },
    );
    setSubmitting(false);
    if (ok) onClose();
  };

  const options = [
    ...groups.map((g) => ({ value: g.id, label: g.name })),
    { value: NEW, label: "➕ New group…" },
  ];

  return (
    <Modal opened={opened} onClose={onClose} title="Add domain to group" centered>
      <Stack gap="sm">
        <Alert variant="light" icon={<IconWorld size={16} />}>
          <Text ff="monospace" fw={600}>
            {domain}
          </Text>
          <Text size="xs" c="dimmed">
            Adding it to a group lets any workspace with that group reach it.
          </Text>
        </Alert>
        <NativeSelect
          label="Target group"
          data={options}
          value={target}
          onChange={(e) => setTarget(e.currentTarget.value)}
        />
        {target === NEW && (
          <TextInput
            label="New group name"
            placeholder="node-pkgs"
            value={newName}
            onChange={(e) => setNewName(e.currentTarget.value)}
            data-autofocus
          />
        )}
        <Group justify="flex-end" mt="xs">
          <Button variant="subtle" onClick={onClose}>
            Cancel
          </Button>
          <Button
            onClick={() => void submit()}
            loading={submitting}
            disabled={target === NEW && !newName.trim()}
            leftSection={<IconCheck size={16} />}
          >
            Add &amp; allow
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
