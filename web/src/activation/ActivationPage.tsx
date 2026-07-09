// ActivationPage is the public activation flow for one workspace, reached via
// its one-time /auth/{token} link. It renders entirely from the JSON state at
// /auth/{token}/state, which the hub gates: an unauthenticated visitor (when
// activation auth is on) sees only the sign-in buttons; an authorized one gets
// the two-step activate flow (open the Claude consent page, paste the code
// back), and a ready workspace shows its session link.
//
// Some elements keep the legacy hooks the browser e2e suite drives: the
// authorize link's `btn-link` class, the `code` input name, the `#activate`
// button, and the `.banner.ok` / `.result` result markup.
import { useEffect, useState } from "react";
import {
  Alert,
  Anchor,
  Button,
  Card,
  Center,
  Divider,
  Group,
  Loader,
  Stack,
  Text,
  TextInput,
  ThemeIcon,
  Title,
} from "@mantine/core";
import { Brand } from "../components/Brand";
import {
  type AuthState,
  fetchAuthState,
  submitCode,
  tokenFromPath,
} from "./api";

/** ActivationPage renders the activation flow for the token in the page URL.
 *
 * @return JSX.Element The activation page.
 */
export function ActivationPage(): JSX.Element {
  const token = tokenFromPath(window.location.pathname);
  const [state, setState] = useState<AuthState | null>(null);
  const [loadError, setLoadError] = useState("");

  useEffect(() => {
    fetchAuthState(token)
      .then(setState)
      .catch((err: Error) => setLoadError(err.message));
  }, [token]);

  return (
    <Center mih="100vh" p="md">
      <Card withBorder radius="lg" padding="xl" w="100%" maw={480}>
        <Stack gap="md">
          <Brand />
          {state?.box_id && (
            <Alert color="blue" py="xs" data-box-meta>
              Workspace <Text span fw={600} ff="monospace">{state.box_id}</Text>
              {state.spoke && (
                <>
                  {" · runner "}
                  <Text span fw={600} ff="monospace">{state.spoke}</Text>
                </>
              )}
            </Alert>
          )}
          {loadError ? (
            <Alert color="red" className="banner err">{loadError}</Alert>
          ) : state === null ? (
            <Group gap="sm">
              <Loader size="sm" />
              <Text c="dimmed" size="sm">Loading…</Text>
            </Group>
          ) : (
            <ActivationBody token={token} state={state} onState={setState} />
          )}
        </Stack>
      </Card>
    </Center>
  );
}

interface BodyProps {
  token: string;
  state: AuthState;
  onState: (s: AuthState) => void;
}

/** ActivationBody renders the state-dependent part of the page: sign-in
 * gating, the ready view, or the two-step activation flow. */
function ActivationBody({ token, state, onState }: BodyProps): JSX.Element {
  if (state.auth_enabled && !state.logged_in) {
    return <SignInGate state={state} />;
  }
  if (state.status === "ready") {
    return <ReadyView state={state} />;
  }
  return <ActivateFlow token={token} state={state} onState={onState} />;
}

/** SignInGate is what a visitor who may not (yet) activate sees: sign-in
 * buttons, or the not-authorized notice for a signed-in non-activator. */
function SignInGate({ state }: { state: AuthState }): JSX.Element {
  if (state.not_authorized) {
    return (
      <Stack gap="sm">
        <Title order={2}>Not authorized to activate</Title>
        <Alert color="red" className="banner err">
          You're signed in as {state.email}, but that account isn't authorized to
          activate workspaces here.
        </Alert>
        <Text c="dimmed" size="sm">
          If this is a mistake, ask an administrator to add your address to the
          activation allow-list.
        </Text>
      </Stack>
    );
  }
  return (
    <Stack gap="sm">
      <Title order={2}>Sign in to activate</Title>
      <Text c="dimmed" size="sm">
        Activating this workspace requires signing in first, so only you — the
        person who requested it — can connect it to a Claude account.
      </Text>
      {(state.providers ?? []).length === 0 ? (
        <Alert color="red" className="banner err">
          No sign-in providers are configured, so this workspace cannot be activated.
        </Alert>
      ) : (
        (state.providers ?? []).map((p) => (
          <Button key={p.login_path} component="a" href={p.login_path} fullWidth>
            Sign in with {p.label}
          </Button>
        ))
      )}
      <Text c="dimmed" size="xs">You'll be returned here once you're signed in.</Text>
    </Stack>
  );
}

/** ReadyView celebrates a completed activation and links the live session. */
function ReadyView({ state }: { state: AuthState }): JSX.Element {
  return (
    <Stack gap="sm">
      <Alert color="teal" className="banner ok">
        <Text fw={700} span>Your llmbox is ready.</Text>
      </Alert>
      {state.session_url ? (
        <div className="result">
          <Text c="dimmed" size="sm">Drive your workspace from here:</Text>
          <Anchor href={state.session_url} target="_blank" rel="noopener noreferrer" style={{ wordBreak: "break-all" }} ff="monospace" size="sm">
            {state.session_url}
          </Anchor>
        </div>
      ) : (
        <Text c="dimmed" size="sm">
          Authentication succeeded — your workspace is now active.
        </Text>
      )}
      {state.email && <SignedInNote email={state.email} />}
    </Stack>
  );
}

/** ActivateFlow is the two-step pending view: open the Claude consent page,
 * then paste the code back and submit it. */
function ActivateFlow({ token, state, onState }: BodyProps): JSX.Element {
  const [code, setCode] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState("");

  const submit = async () => {
    setSubmitting(true);
    setSubmitError("");
    try {
      onState(await submitCode(token, code, state.csrf ?? ""));
    } catch (err) {
      setSubmitError((err as Error).message);
    }
    setSubmitting(false);
  };

  const errMsg = submitError || state.error;
  return (
    <Stack gap="md">
      <div>
        <Title order={2}>Activate your llmbox</Title>
        <Text c="dimmed" size="sm">
          Connect your workspace to your own Claude account. Two quick steps.
        </Text>
      </div>

      {errMsg && (
        <Alert color="red" className="banner err">
          <Text span fw={700}>That didn't work:</Text> {errMsg}
          <br />
          The code may have been mistyped or expired — get a fresh one and try again.
        </Alert>
      )}

      <Divider />
      <Step n={1} title="Sign in with Claude">
        <Text c="dimmed" size="sm">
          Open the Claude sign-in page, approve access, then copy the code it shows you.
        </Text>
        <Center>
          <Button
            component="a"
            className="btn-link"
            href={state.authorize_url}
            target="_blank"
            rel="noopener noreferrer"
          >
            Sign in with Claude →
          </Button>
        </Center>
      </Step>

      <Divider />
      <Step n={2} title="Paste the code">
        <Text c="dimmed" size="sm">
          Paste the code from the previous step to activate your workspace.
        </Text>
        <form
          id="auth-form"
          onSubmit={(e) => {
            e.preventDefault();
            void submit();
          }}
        >
          <Stack gap="sm">
            <TextInput
              name="code"
              placeholder="Paste your code here"
              value={code}
              onChange={(e) => setCode(e.currentTarget.value)}
              autoComplete="off"
              autoCapitalize="off"
              spellCheck={false}
              data-autofocus
            />
            <Button id="activate" type="submit" loading={submitting} fullWidth>
              Activate llmbox
            </Button>
          </Stack>
        </form>
        {submitting && (
          <Group gap="xs">
            <Loader size="xs" />
            <Text c="dimmed" size="sm">
              Contacting Claude and starting your workspace — this can take up to a minute.
            </Text>
          </Group>
        )}
        <Alert color="blue" py="xs">
          <Text size="xs">
            Your code goes straight to this server and into your private workspace —
            it is never sent to the chatbot.
          </Text>
        </Alert>
      </Step>
      {state.email && <SignedInNote email={state.email} />}
    </Stack>
  );
}

/** Step renders one numbered instruction block. */
function Step({ n, title, children }: { n: number; title: string; children: React.ReactNode }): JSX.Element {
  return (
    <Stack gap="sm">
      <Group gap="sm">
        <ThemeIcon radius="xl" size="md">{n}</ThemeIcon>
        <Title order={4}>{title}</Title>
      </Group>
      {children}
    </Stack>
  );
}

/** SignedInNote shows the signed-in identity at the foot of the page. */
function SignedInNote({ email }: { email: string }): JSX.Element {
  return (
    <Text c="dimmed" size="xs">
      Signed in as {email}.
    </Text>
  );
}
