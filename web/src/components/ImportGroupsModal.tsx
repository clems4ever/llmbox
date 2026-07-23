// ImportGroupsModal adds allowlist groups from a portable JSON bundle (the same
// shape the Export button produces). On a name conflict the operator picks merge
// (union the domains into the existing group) or replace (overwrite it).
import { useEffect, useState } from "react";
import { Alert, Button, Group, Modal, NativeSelect, Stack, Textarea } from "@mantine/core";
import { IconInfoCircle, IconUpload } from "@tabler/icons-react";
import type { Api, AllowlistBundle } from "../api";
import { perform } from "../lib/actions";

export interface ImportGroupsModalProps {
  api: Api;
  opened: boolean;
  onClose: () => void;
  onDone: () => Promise<void>;
}

const SAMPLE = `{
  "version": 1,
  "groups": [
    { "name": "node-pkgs", "domains": ["registry.npmjs.org", "*.npmjs.org"], "ttl_seconds": 30 }
  ]
}`;

/** ImportGroupsModal renders the paste-a-bundle import form.
 *
 * @arg props The api client, open state, and callbacks.
 * @return JSX.Element The modal.
 */
export function ImportGroupsModal({ api, opened, onClose, onDone }: ImportGroupsModalProps): JSX.Element {
  const [text, setText] = useState("");
  const [mode, setMode] = useState<"merge" | "replace">("merge");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (opened) {
      setText("");
      setError(null);
      setMode("merge");
    }
  }, [opened]);

  const submit = async () => {
    let bundle: AllowlistBundle;
    try {
      bundle = JSON.parse(text) as AllowlistBundle;
    } catch {
      setError("That isn't valid JSON. Paste a bundle exported from llmbox.");
      return;
    }
    if (!bundle || !Array.isArray(bundle.groups)) {
      setError('The bundle needs a "groups" array.');
      return;
    }
    setError(null);
    setSubmitting(true);
    const ok = await perform(
      async () => {
        const n = await api.importAllowlistGroups(bundle, mode);
        return n;
      },
      { success: "imported groups", onDone },
    );
    setSubmitting(false);
    if (ok) onClose();
  };

  return (
    <Modal opened={opened} onClose={onClose} title="Import groups" centered size="lg">
      <Stack gap="sm">
        <Alert variant="light" icon={<IconInfoCircle size={16} />}>
          Paste a JSON bundle exported from llmbox. Groups with a name that already exists are merged or
          replaced per your choice below.
        </Alert>
        <Textarea
          label="Bundle (JSON)"
          placeholder={SAMPLE}
          autosize
          minRows={7}
          maxRows={14}
          styles={{ input: { fontFamily: "var(--mantine-font-family-monospace)" } }}
          value={text}
          onChange={(e) => setText(e.currentTarget.value)}
          error={error}
        />
        <NativeSelect
          label="On name conflict"
          data={[
            { value: "merge", label: "Merge domains into the existing group" },
            { value: "replace", label: "Replace the existing group" },
          ]}
          value={mode}
          onChange={(e) => setMode(e.currentTarget.value as "merge" | "replace")}
        />
        <Group justify="flex-end" mt="xs">
          <Button variant="subtle" onClick={onClose}>
            Cancel
          </Button>
          <Button
            onClick={() => void submit()}
            loading={submitting}
            disabled={!text.trim()}
            leftSection={<IconUpload size={16} />}
          >
            Import
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
