// GroupEditorModal creates or edits one allowlist group: its name, description,
// the egress domains it permits (exact hosts or leading-wildcard patterns), the
// resolved-IP TTL, and whether it applies to every workspace. A null group is a
// create; a set group pre-fills the form for an update.
import { useEffect, useState } from "react";
import {
  Button,
  Group,
  Modal,
  NumberInput,
  Stack,
  Switch,
  TagsInput,
  Text,
  TextInput,
} from "@mantine/core";
import { IconCheck } from "@tabler/icons-react";
import type { Api, AllowlistGroup } from "../api";
import { perform } from "../lib/actions";

export interface GroupEditorModalProps {
  api: Api;
  /** The group to edit, or null to create a new one. */
  group: AllowlistGroup | null;
  opened: boolean;
  onClose: () => void;
  onSaved: () => Promise<void>;
}

const DEFAULT_TTL = 30;

/** GroupEditorModal renders the create/edit form for an allowlist group.
 *
 * @arg props The api client, the group (or null), open state, and callbacks.
 * @return JSX.Element The modal.
 */
export function GroupEditorModal({
  api,
  group,
  opened,
  onClose,
  onSaved,
}: GroupEditorModalProps): JSX.Element {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [domains, setDomains] = useState<string[]>([]);
  const [ttl, setTtl] = useState<number | "">(DEFAULT_TTL);
  const [isGlobal, setIsGlobal] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  // Re-seed the form whenever the modal opens for a (possibly different) group.
  useEffect(() => {
    if (!opened) return;
    setName(group?.name ?? "");
    setDescription(group?.description ?? "");
    setDomains(group?.domains ?? []);
    setTtl(group?.ttl_seconds ?? DEFAULT_TTL);
    setIsGlobal(group?.is_global ?? false);
  }, [opened, group]);

  const submit = async () => {
    setSubmitting(true);
    const ok = await perform(
      () =>
        api.saveAllowlistGroup({
          id: group?.id,
          name: name.trim(),
          description: description.trim(),
          ttl_seconds: typeof ttl === "number" ? ttl : DEFAULT_TTL,
          is_global: isGlobal,
          domains,
        }),
      { success: group ? `updated ${name.trim()}` : `created ${name.trim()}`, onDone: onSaved },
    );
    setSubmitting(false);
    if (ok) onClose();
  };

  return (
    <Modal opened={opened} onClose={onClose} title={group ? `Edit group · ${group.name}` : "New allowlist group"} centered>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        <Stack gap="sm">
          <TextInput
            required
            label="Name"
            placeholder="github"
            value={name}
            onChange={(e) => setName(e.currentTarget.value)}
            data-autofocus
          />
          <TextInput
            label="Description"
            placeholder="Optional"
            value={description}
            onChange={(e) => setDescription(e.currentTarget.value)}
          />
          <TagsInput
            label="Domains"
            description="Exact hosts or leading wildcards, e.g. github.com or *.github.com. Press Enter to add."
            placeholder="add a domain…"
            value={domains}
            onChange={setDomains}
            splitChars={[",", " "]}
            clearable
          />
          <NumberInput
            label="Resolved-IP TTL (seconds)"
            description="How long a DNS-resolved IP stays open after a lookup before it must be re-resolved. Guards against IP reallocation to rogue services."
            min={1}
            value={ttl}
            onChange={(v) => setTtl(typeof v === "number" ? v : "")}
          />
          <Switch
            label="Apply to all workspaces (global)"
            description="Global groups are reachable by every workspace on an isolation-enabled runner."
            checked={isGlobal}
            onChange={(e) => setIsGlobal(e.currentTarget.checked)}
          />
          {domains.length === 0 && (
            <Text c="dimmed" size="xs">
              A group with no domains reaches nothing — add at least one to make it useful.
            </Text>
          )}
          <Group justify="flex-end" mt="xs">
            <Button variant="subtle" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" loading={submitting} disabled={!name.trim()} leftSection={<IconCheck size={16} />}>
              {group ? "Save" : "Create group"}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
