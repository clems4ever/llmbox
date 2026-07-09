// CreateWorkspaceModal is the "New workspace" form. On submit it creates the box
// and resolves its one-time activation link, then swaps to a result panel that
// shows the link with copy/open affordances — the link is the whole point, since
// a freshly created workspace stays pending until someone opens it. Closing the
// result refreshes the dashboard so the new (pending) workspace appears.
import { useState } from "react";
import {
  Anchor,
  Button,
  CopyButton,
  Group,
  Modal,
  NativeSelect,
  Stack,
  Text,
  TextInput,
} from "@mantine/core";
import { IconCheck, IconCopy, IconExternalLink } from "@tabler/icons-react";
import type { Api, SpokeStatus } from "../api";
import { perform } from "../lib/actions";

export interface CreateWorkspaceModalProps {
  api: Api;
  spokes: SpokeStatus[];
  opened: boolean;
  onClose: () => void;
  refresh: () => Promise<void>;
}

/** defaultSpokeLabel names the "" (server-default) spoke option, naming the
 * current default spoke when one is marked.
 *
 * @arg spokes The known spokes.
 * @return string The label for the default option.
 */
function defaultSpokeLabel(spokes: SpokeStatus[]): string {
  const def = spokes.find((s) => s.default);
  return def ? `default (${def.name})` : "default";
}

/** CreateWorkspaceModal renders the create-workspace form and result panel.
 *
 * @arg props The api client, spoke options, open state, and callbacks.
 * @return JSX.Element The modal.
 */
export function CreateWorkspaceModal({
  api,
  spokes,
  opened,
  onClose,
  refresh,
}: CreateWorkspaceModalProps): JSX.Element {
  const [id, setId] = useState("");
  const [description, setDescription] = useState("");
  const [spoke, setSpoke] = useState<string>("");
  const [submitting, setSubmitting] = useState(false);
  const [created, setCreated] = useState<{ boxId: string; url: string } | null>(null);

  const reset = () => {
    setId("");
    setDescription("");
    setSpoke("");
    setCreated(null);
  };

  const close = () => {
    onClose();
    reset();
  };

  const spokeOptions = [
    { value: "", label: defaultSpokeLabel(spokes) },
    ...spokes.filter((s) => s.connected).map((s) => ({ value: s.name, label: s.name })),
  ];

  const submit = async () => {
    setSubmitting(true);
    await perform(
      async () => {
        const c = await api.createBox(id.trim(), description.trim(), spoke);
        const url = await api.authPageURL(c.token);
        setCreated({ boxId: c.box_id, url });
      },
      { onDone: refresh },
    );
    setSubmitting(false);
  };

  return (
    <Modal opened={opened} onClose={close} title="New workspace" centered>
      {created ? (
        <Stack gap="sm">
          <Text size="sm">
            Workspace <Text span fw={600} ff="monospace">{created.boxId}</Text> was created.
            Open its activation link to authenticate it — it stays pending until then.
          </Text>
          <TextInput
            readOnly
            label="Activation link"
            value={created.url}
            aria-label="Activation link"
          />
          <Group justify="flex-end">
            <CopyButton value={created.url}>
              {({ copied, copy }) => (
                <Button
                  variant="default"
                  leftSection={copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
                  onClick={copy}
                >
                  {copied ? "Copied" : "Copy link"}
                </Button>
              )}
            </CopyButton>
            <Anchor href={created.url} target="_blank" rel="noopener">
              <Button leftSection={<IconExternalLink size={16} />}>Open</Button>
            </Anchor>
            <Button variant="subtle" onClick={close}>
              Done
            </Button>
          </Group>
        </Stack>
      ) : (
        <form
          id="create-box-form"
          onSubmit={(e) => {
            e.preventDefault();
            void submit();
          }}
        >
          <Stack gap="sm">
            <TextInput
              required
              label="Workspace ID"
              name="box_id"
              placeholder="refactor-auth"
              value={id}
              onChange={(e) => setId(e.currentTarget.value)}
              data-autofocus
            />
            <TextInput
              label="Description"
              name="description"
              placeholder="Optional"
              value={description}
              onChange={(e) => setDescription(e.currentTarget.value)}
            />
            <NativeSelect
              label="Spoke"
              name="spoke"
              data={spokeOptions}
              value={spoke}
              onChange={(e) => setSpoke(e.currentTarget.value)}
            />
            <Group justify="flex-end" mt="xs">
              <Button variant="subtle" onClick={close}>
                Cancel
              </Button>
              <Button type="submit" loading={submitting} disabled={!id.trim()}>
                Create workspace
              </Button>
            </Group>
          </Stack>
        </form>
      )}
    </Modal>
  );
}
