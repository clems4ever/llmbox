// InfrastructureView is the operator-facing screen: the runners that host
// workspaces, plus the outstanding join tokens that let new runners enroll. It
// offers make-default / drop on runners and revoke on tokens, and hosts the
// create-runner modal. This is the "plumbing" behind the workspaces list.
// ("Runner" is the UI name for what the architecture, CLI, and API call a
// spoke — the llmbox-spoke agent that dials the hub.)
import { useState } from "react";
import {
  ActionIcon,
  Badge,
  Box,
  Button,
  Group,
  Modal,
  Paper,
  Skeleton,
  Stack,
  Table,
  Text,
  Title,
  Tooltip,
} from "@mantine/core";
import { IconInfoCircle, IconPlus, IconStar, IconTrash } from "@tabler/icons-react";
import type { Api, JoinTokenInfo, SpokeEnrollment, SpokeStatus } from "../api";
import type { DashboardData } from "../lib/data";
import { isExpired, shortTime } from "../lib/format";
import { perform } from "../lib/actions";
import { confirmDestroy } from "../lib/confirm";
import { CreateSpokeModal } from "./CreateSpokeModal";
import { SpokeSetupTabs } from "./SpokeSetupTabs";

export interface InfrastructureViewProps {
  api: Api;
  data: DashboardData | null;
  refresh: () => Promise<void>;
}

/** InfrastructureView renders the spokes and join-tokens management screen.
 *
 * @arg props Dashboard data, the api client, and the refresh callback.
 * @return JSX.Element The infrastructure screen.
 */
export function InfrastructureView({
  api,
  data,
  refresh,
}: InfrastructureViewProps): JSX.Element {
  const [createOpen, setCreateOpen] = useState(false);

  return (
    <Stack gap="lg">
      <Group justify="space-between" align="flex-end" wrap="wrap">
        <Box>
          <Title order={2}>Infrastructure</Title>
          <Text c="dimmed" size="sm">
            Runners host your workspaces; join tokens let new runners enroll.
          </Text>
        </Box>
        <Button leftSection={<IconPlus size={16} />} onClick={() => setCreateOpen(true)}>
          New runner
        </Button>
      </Group>

      {data === null ? (
        <Skeleton height={160} radius="md" />
      ) : (
        <>
          <SpokesCard api={api} spokes={data.spokes} refresh={refresh} />
          {data.tokens.length > 0 && (
            <TokensCard api={api} data={data} refresh={refresh} />
          )}
        </>
      )}

      <CreateSpokeModal
        api={api}
        opened={createOpen}
        onClose={() => setCreateOpen(false)}
        refresh={refresh}
      />
    </Stack>
  );
}

interface SpokesCardProps {
  api: Api;
  spokes: SpokeStatus[];
  refresh: () => Promise<void>;
}

/** SpokesCard renders the spokes table with make-default / drop controls. */
function SpokesCard({ api, spokes, refresh }: SpokesCardProps): JSX.Element {
  const makeDefault = (name: string) =>
    void perform(() => api.setDefaultSpoke(name), {
      success: `default runner is now ${name}`,
      onDone: refresh,
    });

  const drop = (name: string) =>
    confirmDestroy({
      title: "Drop runner",
      message: `Drop runner ${name} and kick its connection?`,
      confirmLabel: "Drop",
      action: () => api.dropSpoke(name),
      success: `dropped runner ${name}`,
      refresh,
    });

  return (
    <Paper withBorder radius="md" id="spokes-card">
      <Group px="md" pt="md" pb="xs">
        <Title order={4}>Runners</Title>
      </Group>
      {spokes.length === 0 ? (
        <Text c="dimmed" size="sm" px="md" pb="md">
          No runners enrolled yet. Create one — it prints a command to run on the runner host.
        </Text>
      ) : (
        <Table.ScrollContainer minWidth={560}>
          <Table verticalSpacing="sm" horizontalSpacing="md" highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Name</Table.Th>
                <Table.Th>Status</Table.Th>
                <Table.Th>Enrolled</Table.Th>
                <Table.Th />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {spokes.map((sp) => (
                <Table.Tr
                  key={sp.name}
                  data-spoke-row={sp.name}
                  data-spoke-status={sp.connected ? "connected" : "offline"}
                >
                  <Table.Td>
                    <Group gap="xs">
                      <Text className="mono-wrap" data-spoke={sp.name}>{sp.name}</Text>
                      {sp.default && <Badge size="sm" variant="light" color="brand">default</Badge>}
                    </Group>
                  </Table.Td>
                  <Table.Td>
                    <Badge variant="light" color={sp.connected ? "teal" : "red"} radius="sm">
                      {sp.connected ? "connected" : "offline"}
                    </Badge>
                  </Table.Td>
                  <Table.Td className="mono-wrap">{shortTime(sp.enrolled_at)}</Table.Td>
                  <Table.Td ta="right">
                    <Group gap="xs" justify="flex-end" wrap="nowrap">
                      {!sp.default && (
                        <Tooltip label="Make default">
                          <ActionIcon
                            variant="subtle"
                            aria-label={`Make ${sp.name} default`}
                            onClick={() => makeDefault(sp.name)}
                          >
                            <IconStar size={16} />
                          </ActionIcon>
                        </Tooltip>
                      )}
                      <Tooltip label="Drop runner">
                        <ActionIcon
                          variant="subtle"
                          color="red"
                          data-spoke-drop={sp.name}
                          aria-label={`Drop ${sp.name}`}
                          onClick={() => drop(sp.name)}
                        >
                          <IconTrash size={16} />
                        </ActionIcon>
                      </Tooltip>
                    </Group>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
    </Paper>
  );
}

interface TokensCardProps {
  api: Api;
  data: DashboardData;
  refresh: () => Promise<void>;
}

/** TokensCard renders the outstanding join tokens with per-token setup
 * instructions (re-shown with a token placeholder, plus a regenerate action
 * that mints a fresh token when the original was lost) and revoke controls. */
function TokensCard({ api, data, refresh }: TokensCardProps): JSX.Element {
  const [setupToken, setSetupToken] = useState<JoinTokenInfo | null>(null);
  // A fresh enrollment minted from the setup modal; its real one-time command
  // replaces the placeholder one until the modal closes.
  const [regenerated, setRegenerated] = useState<SpokeEnrollment | null>(null);
  const [regenerating, setRegenerating] = useState(false);

  const openSetup = (tok: JoinTokenInfo) => {
    setRegenerated(null);
    setSetupToken(tok);
  };

  const regenerate = async (tok: JoinTokenInfo) => {
    setRegenerating(true);
    await perform(
      async () => {
        setRegenerated(await api.regenerateJoinToken(tok.id));
      },
      { success: `minted a fresh token for ${tok.name}`, onDone: refresh },
    );
    setRegenerating(false);
  };

  const revoke = (id: string) =>
    confirmDestroy({
      title: "Revoke join token",
      message: "Revoke this join token? A runner that hasn't enrolled yet can no longer use it.",
      confirmLabel: "Revoke",
      action: () => api.revokeJoinToken(id),
      success: "revoked join token",
      refresh,
    });

  return (
    <Paper withBorder radius="md" id="tokens-card">
      <Group px="md" pt="md" pb="xs">
        <Title order={4}>Outstanding join tokens</Title>
      </Group>
      <Table.ScrollContainer minWidth={560}>
        <Table verticalSpacing="sm" horizontalSpacing="md">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>ID</Table.Th>
              <Table.Th>Runner</Table.Th>
              <Table.Th>Expires</Table.Th>
              <Table.Th />
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {data.tokens.map((tok) => (
              <Table.Tr key={tok.id} data-token-row={tok.id}>
                <Table.Td className="mono-wrap">{tok.id.slice(0, 12)}</Table.Td>
                <Table.Td className="mono-wrap">{tok.name}</Table.Td>
                <Table.Td>
                  <Group gap="xs">
                    <Text className="mono-wrap" size="sm">{shortTime(tok.expires_at)}</Text>
                    {isExpired(tok.expires_at) && (
                      <Badge size="sm" variant="light" color="red">expired</Badge>
                    )}
                  </Group>
                </Table.Td>
                <Table.Td ta="right">
                  <Group gap="xs" justify="flex-end" wrap="nowrap">
                    <Tooltip label="Setup instructions">
                      <ActionIcon
                        variant="subtle"
                        data-token-info={tok.id}
                        aria-label={`Setup instructions for ${tok.name}`}
                        onClick={() => openSetup(tok)}
                      >
                        <IconInfoCircle size={16} />
                      </ActionIcon>
                    </Tooltip>
                    <Tooltip label="Revoke token">
                      <ActionIcon
                        variant="subtle"
                        color="red"
                        data-token-revoke={tok.id}
                        aria-label="Revoke token"
                        onClick={() => revoke(tok.id)}
                      >
                        <IconTrash size={16} />
                      </ActionIcon>
                    </Tooltip>
                  </Group>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>

      <Modal
        opened={setupToken !== null}
        onClose={() => setSetupToken(null)}
        title={setupToken ? `Runner setup — ${setupToken.name}` : ""}
        centered
        size="lg"
      >
        {setupToken && (
          <Stack gap="sm">
            {regenerated && (
              <Text size="sm">
                Fresh token minted — it is shown only once, save it this time.
              </Text>
            )}
            <SpokeSetupTabs
              command={regenerated?.command ?? setupToken.command}
              onRegenerate={() => void regenerate(setupToken)}
              regenerating={regenerating}
            />
          </Stack>
        )}
      </Modal>
    </Paper>
  );
}
