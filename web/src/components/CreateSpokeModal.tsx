// CreateSpokeModal is the "New runner" form (a runner is the UI name for a
// spoke). On submit it enrolls the spoke and swaps to a result panel with the
// setup instructions (run command, or a systemd service script) — the token is
// shown only once, so getting it copied is front and centre. Closing refreshes
// so the pending (offline) runner appears in the list.
import { useState } from "react";
import {
  Button,
  Group,
  Modal,
  NativeSelect,
  Stack,
  Text,
  TextInput,
} from "@mantine/core";
import type { Api } from "../api";
import { perform } from "../lib/actions";
import { SpokeSetupTabs } from "./SpokeSetupTabs";

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
    <Modal opened={opened} onClose={close} title="New runner" centered size="lg">
      {created ? (
        <Stack gap="sm">
          <Text size="sm">
            Set this up on the <Text span fw={600}>{created.name}</Text> host — the token
            is shown only once.
          </Text>
          <SpokeSetupTabs command={created.command} />
          <Group justify="flex-end">
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
                Create runner
              </Button>
            </Group>
          </Stack>
        </form>
      )}
    </Modal>
  );
}
