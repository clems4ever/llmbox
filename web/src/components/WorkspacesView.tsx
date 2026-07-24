// WorkspacesView is the primary screen: the list of workspaces (boxes) with a
// table/grid view toggle, a "New workspace" action, and per-row Details/Remove.
// Selecting a workspace opens its details drawer (owned by Dashboard); creating
// one opens the modal. HTTP proxies are deliberately NOT shown here — they live
// under each workspace's details, since a proxy only means something in the
// context of the box it fronts.
import { useState } from "react";
import {
  ActionIcon,
  Box,
  Button,
  Card,
  Center,
  Group,
  Paper,
  SegmentedControl,
  SimpleGrid,
  Skeleton,
  Stack,
  Table,
  Text,
  Title,
  Tooltip,
} from "@mantine/core";
import {
  IconLayoutGrid,
  IconLayoutList,
  IconPlayerPause,
  IconPlayerPlay,
  IconPlus,
  IconTrash,
} from "@tabler/icons-react";
import type { Api, BoxView } from "../api";
import type { DashboardData } from "../lib/data";
import { boxId, createdAt, lastSeenAt, stateTone } from "../lib/format";
import { confirmDestroy } from "../lib/confirm";
import { perform } from "../lib/actions";
import { StateBadge, StatusBadge } from "./StatusBadge";
import { CreateWorkspaceModal } from "./CreateWorkspaceModal";

export type WorkspaceLayout = "table" | "grid";

export interface WorkspacesViewProps {
  api: Api;
  data: DashboardData | null;
  refresh: () => Promise<void>;
  onSelect: (id: string) => void;
}

/** WorkspacesView renders the workspace list plus its create/remove controls.
 *
 * @arg props Dashboard data, the api client, and selection/refresh callbacks.
 * @return JSX.Element The workspaces screen.
 */
export function WorkspacesView({
  api,
  data,
  refresh,
  onSelect,
}: WorkspacesViewProps): JSX.Element {
  const [layout, setLayout] = useState<WorkspaceLayout>("table");
  const [createOpen, setCreateOpen] = useState(false);

  const remove = (b: BoxView) => {
    const id = boxId(b);
    confirmDestroy({
      title: "Remove workspace",
      message: `Remove workspace ${id}? This destroys the workspace and cannot be undone.`,
      action: () => api.destroyBox(id),
      success: `removed workspace ${id}`,
      refresh,
    });
  };

  // Pause/resume are non-destructive, so they skip the confirm modal: they just run
  // and refresh so the state badge flips running↔paused.
  const pause = (b: BoxView) => {
    const id = boxId(b);
    void perform(() => api.pauseBox(id), { success: `paused workspace ${id}`, onDone: refresh });
  };
  const resume = (b: BoxView) => {
    const id = boxId(b);
    void perform(() => api.resumeBox(id), { success: `resumed workspace ${id}`, onDone: refresh });
  };

  return (
    <Stack gap="lg">
      <Group justify="space-between" align="flex-end" wrap="wrap">
        <Box>
          <Title order={2}>Workspaces</Title>
          <Text c="dimmed" size="sm">
            Isolated workspaces running on your runners. Select one to manage its HTTP proxies.
          </Text>
        </Box>
        <Group gap="sm">
          <SegmentedControl
            value={layout}
            onChange={(v) => setLayout(v as WorkspaceLayout)}
            aria-label="View layout"
            data={[
              // Icon-only labels must be wrapped in <Center>: a bare inline SVG
              // sits on the text baseline and renders off-center in the segment.
              {
                value: "table",
                label: (
                  <Center>
                    <IconLayoutList size={16} aria-label="Table view" />
                  </Center>
                ),
              },
              {
                value: "grid",
                label: (
                  <Center>
                    <IconLayoutGrid size={16} aria-label="Grid view" />
                  </Center>
                ),
              },
            ]}
          />
          <Button leftSection={<IconPlus size={16} />} onClick={() => setCreateOpen(true)}>
            New workspace
          </Button>
        </Group>
      </Group>

      {data === null ? (
        <Stack gap="xs">
          <Skeleton height={44} radius="sm" />
          <Skeleton height={44} radius="sm" />
          <Skeleton height={44} radius="sm" />
        </Stack>
      ) : data.boxes.length === 0 ? (
        <EmptyWorkspaces onCreate={() => setCreateOpen(true)} />
      ) : layout === "table" ? (
        <WorkspaceTable boxes={data.boxes} onSelect={onSelect} onRemove={remove} onPause={pause} onResume={resume} />
      ) : (
        <WorkspaceGrid boxes={data.boxes} onSelect={onSelect} onRemove={remove} onPause={pause} onResume={resume} />
      )}

      <CreateWorkspaceModal
        api={api}
        spokes={data?.spokes ?? []}
        opened={createOpen}
        onClose={() => setCreateOpen(false)}
        refresh={refresh}
      />
    </Stack>
  );
}

interface RowProps {
  boxes: BoxView[];
  onSelect: (id: string) => void;
  onRemove: (b: BoxView) => void;
  onPause: (b: BoxView) => void;
  onResume: (b: BoxView) => void;
}

/** WorkspacePauseAction renders the per-box pause/resume control: a Resume button
 * for a paused box, a Start button for a stopped one (a box whose VMM died — e.g.
 * after a host reboot or a spoke/hub upgrade — so recovery doesn't require the
 * CLI), a Pause button for a running one, and nothing for a box in any other
 * state (unreachable, terminated) where none applies. Resume and Start both go
 * through the same resume-box endpoint, which boots the box from its persisted
 * disk; the label differs only so the affordance matches the box's state. */
function WorkspacePauseAction({
  box,
  onPause,
  onResume,
}: {
  box: BoxView;
  onPause: (b: BoxView) => void;
  onResume: (b: BoxView) => void;
}): JSX.Element | null {
  const id = boxId(box);
  const tone = stateTone(box.state);
  if (tone === "paused" || tone === "stopped") {
    // A paused box is "resumed"; a stopped one is "started" — same endpoint,
    // different word so the button reads right for the state it's offered on.
    const verb = tone === "paused" ? "Resume" : "Start";
    return (
      <Tooltip label={`${verb} workspace`}>
        <ActionIcon
          variant="subtle"
          color="teal"
          data-box-resume={id}
          aria-label={`${verb} ${id}`}
          onClick={() => onResume(box)}
        >
          <IconPlayerPlay size={16} />
        </ActionIcon>
      </Tooltip>
    );
  }
  // Only a running box can be paused; pausing an offline box makes no sense.
  if (box.state === "running") {
    return (
      <Tooltip label="Pause workspace to save compute">
        <ActionIcon
          variant="subtle"
          color="grape"
          data-box-pause={id}
          aria-label={`Pause ${id}`}
          onClick={() => onPause(box)}
        >
          <IconPlayerPause size={16} />
        </ActionIcon>
      </Tooltip>
    );
  }
  return null;
}

/** WorkspaceTable renders the dense, sortable-looking table view. */
function WorkspaceTable({ boxes, onSelect, onRemove, onPause, onResume }: RowProps): JSX.Element {
  return (
    <Paper withBorder radius="md" id="boxes-card">
      <Table.ScrollContainer minWidth={720}>
        <Table highlightOnHover verticalSpacing="sm" horizontalSpacing="md">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Workspace</Table.Th>
              <Table.Th>Runner</Table.Th>
              <Table.Th>Image</Table.Th>
              <Table.Th>State</Table.Th>
              <Table.Th>Phase</Table.Th>
              <Table.Th />
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {boxes.map((b) => {
              const id = boxId(b);
              return (
                <Table.Tr
                  key={id}
                  data-box-row={id}
                  style={{ cursor: "pointer" }}
                  onClick={() => onSelect(id)}
                >
                  <Table.Td>
                    <Text fw={600} className="mono-wrap">{id}</Text>
                    {b.description && (
                      <Text c="dimmed" size="xs">{b.description}</Text>
                    )}
                  </Table.Td>
                  <Table.Td className="mono-wrap" data-box-spoke={id}>{b.spoke ?? ""}</Table.Td>
                  <Table.Td className="mono-wrap">{b.image}</Table.Td>
                  <Table.Td>
                    <StateBadge state={b.state} lastSeen={b.last_seen} />
                    {stateTone(b.state) === "unreachable" && lastSeenAt(b.last_seen) && (
                      <Text c="dimmed" size="xs">last seen {lastSeenAt(b.last_seen)}</Text>
                    )}
                  </Table.Td>
                  <Table.Td><StatusBadge phase={b.phase} /></Table.Td>
                  <Table.Td onClick={(e) => e.stopPropagation()} ta="right">
                    <Group gap={4} justify="flex-end" wrap="nowrap">
                      <WorkspacePauseAction box={b} onPause={onPause} onResume={onResume} />
                      <Tooltip label="Remove workspace">
                        <ActionIcon
                          variant="subtle"
                          color="red"
                          data-box={id}
                          aria-label={`Remove ${id}`}
                          onClick={() => onRemove(b)}
                        >
                          <IconTrash size={16} />
                        </ActionIcon>
                      </Tooltip>
                    </Group>
                  </Table.Td>
                </Table.Tr>
              );
            })}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
    </Paper>
  );
}

/** WorkspaceGrid renders the roomier card view. */
function WorkspaceGrid({ boxes, onSelect, onRemove, onPause, onResume }: RowProps): JSX.Element {
  return (
    <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} id="boxes-card">
      {boxes.map((b) => {
        const id = boxId(b);
        return (
          <Card
            key={id}
            withBorder
            radius="md"
            padding="md"
            data-box-row={id}
            style={{ cursor: "pointer" }}
            onClick={() => onSelect(id)}
          >
            <Group justify="space-between" wrap="nowrap" mb="xs">
              <Text fw={600} className="mono-wrap">{id}</Text>
              <StatusBadge phase={b.phase} />
            </Group>
            {b.description && (
              <Text c="dimmed" size="sm" lineClamp={2} mb="xs">{b.description}</Text>
            )}
            <Stack gap={4} mb="sm">
              <Group gap="xs">
                <StateBadge state={b.state} lastSeen={b.last_seen} />
                {b.spoke && <Text size="xs" c="dimmed" data-box-spoke={id}>on {b.spoke}</Text>}
              </Group>
              {b.created > 0 && (
                <Text size="xs" c="dimmed">created {createdAt(b.created)}</Text>
              )}
              {stateTone(b.state) === "unreachable" && lastSeenAt(b.last_seen) && (
                <Text size="xs" c="dimmed">last seen {lastSeenAt(b.last_seen)}</Text>
              )}
            </Stack>
            <Group justify="flex-end" onClick={(e) => e.stopPropagation()}>
              <Group gap={4} wrap="nowrap">
                <WorkspacePauseAction box={b} onPause={onPause} onResume={onResume} />
                <Tooltip label="Remove workspace">
                  <ActionIcon
                    variant="subtle"
                    color="red"
                    data-box={id}
                    aria-label={`Remove ${id}`}
                    onClick={() => onRemove(b)}
                  >
                    <IconTrash size={16} />
                  </ActionIcon>
                </Tooltip>
              </Group>
            </Group>
          </Card>
        );
      })}
    </SimpleGrid>
  );
}

/** EmptyWorkspaces is the zero-state prompt shown when no workspaces exist. */
function EmptyWorkspaces({ onCreate }: { onCreate: () => void }): JSX.Element {
  return (
    <Paper withBorder radius="md" p="xl">
      <Stack align="center" gap="sm">
        <Text fw={600}>No workspaces yet</Text>
        <Text c="dimmed" size="sm" ta="center">
          Create your first workspace to spin up an isolated environment on one of your runners.
        </Text>
        <Button leftSection={<IconPlus size={16} />} onClick={onCreate}>
          New workspace
        </Button>
      </Stack>
    </Paper>
  );
}
