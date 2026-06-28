# Development

Building, CI, and the test suites.

## Building & common tasks

A [`Makefile`](../Makefile) wraps the common tasks — run `make help` to list them.
The most-used:

```bash
make build              # build ./llmbox
make run CONFIG=llmbox.yaml   # run the server against a config file
make check              # gofmt-check + go vet + unit tests
make cover              # unit tests with a coverage total
make test-integration   # integration tests (needs Docker + a Claude binary)
make test-e2e           # end-to-end workflow test (needs Chrome + chromedriver)
make docker-build       # build the Docker image
```

## CI

`.github/workflows/ci.yml` runs `go vet` and the unit-test suite with coverage on
every push and pull request, publishing the coverage badge. It also runs the
end-to-end workflow test in a separate `e2e` job (headless Chrome via WebDriver),
kept apart from the fast unit suite so developers can run either independently —
`make test` for the unit tests, `make test-e2e` for the workflow test.
`.github/workflows/docker.yml` builds the server image and pushes it to GitHub
Container Registry (`ghcr.io/<owner>/llmbox`) on pushes to `main` and version
tags. Pull requests build without pushing.

The Claude Code binary baked into the image is **pinned** to a specific stable
release — the `ARG CLAUDE_VERSION` line in the [`Dockerfile`](../Dockerfile) is the
single source of truth. `.github/workflows/bump-claude.yml` runs daily, resolves
the latest stable release from `downloads.claude.ai`, and opens a PR bumping that
line when a newer version is available. Override per build with
`docker build --build-arg CLAUDE_VERSION=<x.y.z|stable|latest> .`.

## Testing

`go test ./...` covers the Docker layer (a faked Docker client; the attach
stream is driven over an in-memory pipe) and the server (MCP tools over an
in-memory transport + the auth web handlers). An integration test against a real
container is gated behind a build tag:

```bash
go test -tags=integration -run Integration -v ./internal/docker/
```

It confirms a live `claude auth login` emits a real OAuth authorize URL (PKCE +
out-of-band code callback) that the manager captures.

An **end-to-end workflow test** lives under [`e2e/`](../e2e/) behind the `e2e` build
tag. It runs the real server (MCP tools + the auth web UI) on a real HTTP
listener and exercises the whole activation flow — chatbot creates a box over
MCP, a human opens the auth page, "signs in with Claude", approves access,
copies the one-time code, pastes it in, and the box goes ready — driving the
chatbot side over a real MCP client and the human side through a real headless
Chrome via [WebDriver](https://github.com/tebeka/selenium). Only the two external
dependencies are simulated: the Docker box layer (an in-memory box manager) and
the Anthropic OAuth platform (an in-process consent server). It also asserts the
core security property: the OAuth URL and code never appear in any MCP output.

```bash
make test-e2e            # or: go test -tags e2e ./e2e/...
```

The e2e suite is opt-in: it only builds under `-tags e2e`, so the default
`go test ./...` unit run never includes it and stays fast. When you do run it and
no `chromedriver` is available (on `$CHROMEWEBDRIVER` or `$PATH`), it **fails**
rather than skipping — running the suite is an explicit request, so a missing
browser is an error, not a silent pass.

When `$LLMBOX_E2E_SCREENSHOT_DIR` is set, the tests also save PNG screenshots of
the auth page (`auth-page.png`, `auth-ready.png`, and a phone-sized
`auth-page-mobile.png`) and of the proxy sign-in page (`signin-page.png` and the
phone-sized `signin-page-mobile.png`) to that directory. CI sets it
to [`.github/screenshots/`](../.github/screenshots) and, **on pull requests** (from
this repo), commits the refreshed images straight onto the PR branch and posts a
sticky comment previewing the changed images inline, so the
[activation page](architecture.md#the-activation-page) and the
[proxy sign-in page](proxy.md#authentication) shown in the docs always match the
live UI and the change is easy to review together with the code that caused it.
