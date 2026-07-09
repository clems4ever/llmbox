// SignInPage is the stand-alone sign-in page an unauthenticated proxy visitor
// is bounced to (/signin?return=…). It renders from GET /signin/state: one
// button per provider, each returning to the sanitized target after login. A
// missing or unsafe target yields no buttons and an explanatory notice — the
// open-redirect guard lives hub-side. An already-signed-in visitor is
// forwarded straight to the return target (the hub also does this server-side
// before the shell is even served).
import { useEffect, useState } from "react";
import { Alert, Button, Card, Center, Loader, Group, Stack, Text, Title } from "@mantine/core";
import { Brand } from "../components/Brand";

interface ProviderButton {
  label: string;
  login_path: string;
}

interface SignInState {
  signed_in: boolean;
  return_to?: string;
  providers?: ProviderButton[];
}

/** SignInPage renders the provider sign-in buttons for the ?return= target.
 *
 * @return JSX.Element The sign-in page.
 */
export function SignInPage(): JSX.Element {
  const [state, setState] = useState<SignInState | null>(null);
  const [loadError, setLoadError] = useState("");

  useEffect(() => {
    const ret = new URLSearchParams(window.location.search).get("return") ?? "";
    fetch(`/signin/state?return=${encodeURIComponent(ret)}`, { credentials: "same-origin" })
      .then(async (resp) => {
        if (!resp.ok) throw new Error(resp.statusText || `HTTP ${resp.status}`);
        const st = (await resp.json()) as SignInState;
        // Belt and braces: the hub already 302s a signed-in visitor with a safe
        // target before serving this shell; this covers signing in mid-visit.
        if (st.signed_in && st.return_to) {
          window.location.replace(st.return_to);
          return;
        }
        setState(st);
      })
      .catch((err: Error) => setLoadError(err.message));
  }, []);

  return (
    <Center mih="100vh" p="md">
      <Card withBorder radius="lg" padding="xl" w="100%" maw={480}>
        <Stack gap="md">
          <Brand />
          <Title order={2}>Sign in</Title>
          {loadError ? (
            <Alert color="red" className="banner err">{loadError}</Alert>
          ) : state === null ? (
            <Group gap="sm">
              <Loader size="sm" />
              <Text c="dimmed" size="sm">Loading…</Text>
            </Group>
          ) : (state.providers ?? []).length > 0 ? (
            <>
              <Text c="dimmed" size="sm">Sign in to continue to this workspace.</Text>
              {(state.providers ?? []).map((p) => (
                <Button key={p.login_path} component="a" href={p.login_path} fullWidth>
                  Sign in with {p.label}
                </Button>
              ))}
              <Text c="dimmed" size="xs">
                You'll be returned to the page you were visiting once you're signed in.
              </Text>
            </>
          ) : (
            <Alert color="red" className="banner err">
              No valid destination to sign in for. Open the workspace's link directly.
            </Alert>
          )}
        </Stack>
      </Card>
    </Center>
  );
}
