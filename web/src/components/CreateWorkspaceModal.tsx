// CreateWorkspaceModal is the "New workspace" form. On submit it creates the
// workspace on the chosen runner, refreshes the dashboard so the new workspace
// appears, and closes. A workspace comes up running once its init script
// succeeds; there is nothing more to do here.
import { useEffect, useState } from "react";
import {
  Badge,
  Button,
  Checkbox,
  Group,
  Input,
  Modal,
  NativeSelect,
  NumberInput,
  Stack,
  Text,
  TextInput,
} from "@mantine/core";
import type { AllowlistGroup, Api, SpokeStatus } from "../api";
import { perform } from "../lib/actions";

/** GiB is the bytes-per-gibibyte factor used to turn the operator-friendly GiB
 * knob in the form into the byte count the create API expects. It mirrors the
 * same constant the server uses. */
const GiB = 1024 * 1024 * 1024;

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

/** CreateWorkspaceModal renders the create-workspace form.
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
  const [diskGiB, setDiskGiB] = useState<number | "">("");
  const [submitting, setSubmitting] = useState(false);
  const [groups, setGroups] = useState<AllowlistGroup[]>([]);
  const [selectedGroups, setSelectedGroups] = useState<string[]>([]);

  // Load the allowlist groups when the modal opens so the operator can pick which
  // extra (non-global) groups the new workspace should reach. Global groups apply
  // automatically and are shown for context.
  useEffect(() => {
    if (!opened) return;
    let cancelled = false;
    void api
      .listAllowlistGroups()
      .then((gs) => {
        if (!cancelled) setGroups(gs);
      })
      .catch(() => {
        // The picker is optional; a failure just hides it rather than blocking creation.
      });
    return () => {
      cancelled = true;
    };
  }, [opened, api]);

  const reset = () => {
    setId("");
    setDescription("");
    setSpoke("");
    setDiskGiB("");
    setSelectedGroups([]);
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
    const boxId = id.trim();
    const diskBytes = typeof diskGiB === "number" && diskGiB > 0 ? Math.round(diskGiB * GiB) : 0;
    const ok = await perform(
      async () => {
        await api.createBox(boxId, description.trim(), spoke, diskBytes);
        // Attach the chosen non-global groups once the workspace exists. Global
        // groups already apply, so only the selected extras are recorded.
        if (selectedGroups.length > 0) await api.setBoxGroups(boxId, selectedGroups);
      },
      {
        success: `created workspace ${boxId}`,
        onDone: refresh,
      },
    );
    setSubmitting(false);
    if (ok) close();
  };

  const optionalGroups = groups.filter((g) => !g.is_global);
  const globalGroups = groups.filter((g) => g.is_global);

  return (
    <Modal opened={opened} onClose={close} title="New workspace" centered>
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
            label="Runner"
            name="spoke"
            data={spokeOptions}
            value={spoke}
            onChange={(e) => setSpoke(e.currentTarget.value)}
          />
          <NumberInput
            label="Disk size (GiB)"
            name="disk_gb"
            placeholder="runner default"
            description="Optional. Honoured only by microVM runners; capped by the runner's configured maximum."
            min={1}
            step={1}
            allowDecimal={false}
            value={diskGiB}
            onChange={(v) => setDiskGiB(typeof v === "number" ? v : "")}
          />
          {groups.length > 0 && (
            <Input.Wrapper
              label="Allowlist groups"
              description="Global groups always apply. Pick any extra groups this workspace should reach — you can change them later."
            >
              <Stack gap={6} mt={6}>
                {globalGroups.map((g) => (
                  <Checkbox
                    key={g.id}
                    checked
                    disabled
                    label={
                      <Group gap={6}>
                        <Text span>{g.name}</Text>
                        <Badge size="xs" variant="light">
                          global
                        </Badge>
                      </Group>
                    }
                  />
                ))}
                {optionalGroups.map((g) => (
                  <Checkbox
                    key={g.id}
                    checked={selectedGroups.includes(g.id)}
                    onChange={(e) => {
                      const on = e.currentTarget.checked;
                      setSelectedGroups((cur) => (on ? [...cur, g.id] : cur.filter((x) => x !== g.id)));
                    }}
                    label={g.name}
                    description={g.description || `${g.domains.length} domains`}
                  />
                ))}
              </Stack>
            </Input.Wrapper>
          )}
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
    </Modal>
  );
}
