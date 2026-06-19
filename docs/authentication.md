# Authenticating activation

The auth-page URL is handed back from `create_llmbox` and therefore travels
**through the chatbot** (claude.ai's servers). The 256-bit token in it is the only
thing gating activation, so anyone who can see that traffic — and reaches the box
before the requester does — can activate the box with **their own** Claude
account, hijacking it and any per-box secrets your [hooks](hooks.md) inject. See
[Status & caveats](../README.md#status--caveats) for the residual gaps.

To close this, enable a sign-in provider under `auth`. Activation then requires
the visitor to authenticate over a channel that **never** touches the chatbot
(OIDC, browser ↔ provider ↔ llmbox) and be in an allowed domain or email
allowlist; an unauthenticated visitor sees only the sign-in buttons, never the
code form or the session URL. Each provider is a dedicated config block so more
can be added later.

```yaml
auth:
  session_ttl: "1h"
  google:
    enabled: true
    client_id: "xxxxxxxx.apps.googleusercontent.com"
    client_secret_file: "/etc/llmbox/google-client-secret"  # secret read from file, never inlined
    allowed_domains: ["your-company.com"]
    allowed_emails: []
```

Setup notes:
- In the Google Cloud console, create an **OAuth 2.0 Client ID** (type *Web
  application*) and register the redirect URI `{public_url}/auth/google/callback`
  (the `redirect_url` field defaults to this).
- The client secret is **read from a file** (`client_secret_file`); it is never
  written in the YAML. Mount it read-only.
- Enabling a provider with no `allowed_domains` **and** no `allowed_emails` is a
  hard error — it would otherwise authorize every Google account.
- Login sessions are persisted server-side (in the `state_file` bbolt DB) so they
  survive restarts; `session_ttl` bounds their lifetime.

When `auth` is omitted, activation is unauthenticated (the server logs a warning
at startup) and behaves as before.
