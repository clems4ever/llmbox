// CreateSpokeModal is the "New spoke" form. On submit it enrolls the spoke and
// swaps to a result panel showing the one-time enrollment command to run on the
// spoke host — the token is shown only once, so copy-to-clipboard is front and
// centre. Closing refreshes so the pending (offline) spoke appears in the list.
import { useState } from "react";
import {
  Button,
  Code,
  CopyButton,
  Group,
  Modal,
  NativeSelect,
  Stack,
  Text,
  TextInput,
} from "@mantine/core";
import { IconCheck, IconCopy } from "@tabler/icons-react";
import type { Api } from "../api";
import { perform } from "../lib/actions";

export interface CreateSpokeModalProps {
  api: Api;
  opened: boolean;
  onClose: () => void;
  refresh: () => Promise<void>;
}

/** CreateSpokeModal renders the create-spoke form and its enrollment result.
 *
 * @arg props The api client, open state, and close/refresh callbacks.
 * @return JSX.Element The modal.
 */
export function CreateSpokeModal({
  api,
  opened,
  onClose,
  refresh,
}: CreateSpokeModalProps): JSX.Element {
  const [name, setName] = useState("");
  const [backend, setBackend] = useState("docker");
  const [ttl, setTtl] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [created, setCreated] = useState<{ name: string; command: string } | null>(null);

  const reset = () => {
    setName("");
    setBackend("docker");
    setTtl("");
    setCreated(null);
  };
  const close = () => {
    onClose();
    reset();
  };

  const submit = async () => {
    setSubmitting(true);
    await perform(
      async () => {
        const sp = await api.createSpoke(name.trim(), backend, ttl.trim());
        setCreated({ name: sp.name, command: sp.command });
      },
      { onDone: refresh },
    );
    setSubmitting(false);
  };

  return (
    <Modal opened={opened} onClose={close} title="New spoke" centered>
      {created ? (
        <Stack gap="sm">
          <Text size="sm">
            Run this on the <Text span fw={600}>{created.name}</Text> host — the token is
            shown only once.
          </Text>
          <Code block>{created.command}</Code>
          <Text c="dimmed" size="xs">
            After first enrollment the spoke reconnects from its saved credential; the
            token is one-time.
          </Text>
          <Group justify="flex-end">
            <CopyButton value={created.command}>
              {({ copied, copy }) => (
                <Button
                  variant="default"
                  leftSection={copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
                  onClick={copy}
                >
                  {copied ? "Copied" : "Copy command"}
                </Button>
              )}
            </CopyButton>
            <Button variant="subtle" onClick={close}>Done</Button>
          </Group>
        </Stack>
      ) : (
        <form
          id="create-spoke-form"
          onSubmit={(e) => {
            e.preventDefault();
            void submit();
          }}
        >
          <Stack gap="sm">
            <TextInput
              required
              label="Name"
              name="name"
              placeholder="edge-1"
              value={name}
              onChange={(e) => setName(e.currentTarget.value)}
              data-autofocus
            />
            <NativeSelect
              label="Backend"
              name="backend"
              data={["docker", "firecracker"]}
              value={backend}
              onChange={(e) => setBackend(e.currentTarget.value)}
            />
            <TextInput
              label="Token TTL"
              name="ttl"
              placeholder="1h (optional)"
              value={ttl}
              onChange={(e) => setTtl(e.currentTarget.value)}
            />
            <Group justify="flex-end" mt="xs">
              <Button variant="subtle" onClick={close}>Cancel</Button>
              <Button type="submit" loading={submitting} disabled={!name.trim()}>
                Create spoke
              </Button>
            </Group>
          </Stack>
        </form>
      )}
    </Modal>
  );
}
