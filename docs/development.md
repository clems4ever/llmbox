# Development

Building, CI, and the test suites.

## Building & common tasks

A [`Makefile`](../Makefile) wraps the common tasks — run `make help` to list them.
The most-used:

```bash
make build              # build ./llmbox-server
make run CONFIG=llmbox.yaml   # run the server against a config file
make check              # gofmt-check + go vet + unit tests
make cover              # unit tests with a coverage total
make test-integration   # integration tests (needs Docker; builds a minimal guest-only box image)
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

The default **box** image ([`Dockerfile.box`](../Dockerfile.box)) is a minimal
infra base — a box's workload is installed and started by the spoke's init script.
The **server** image ([`Dockerfile`](../Dockerfile)) carries only the llmbox
server binary.

## Testing

`go test ./...` covers the box layer (a faked Docker client; the attach stream is
driven over an in-memory pipe) and the hub (MCP tools over an in-memory transport
+ the admin/sign-in web handlers). An integration test that runs the box
conformance contract against a **real** Docker container is gated behind a build
tag:

```bash
go test -tags=integration -run Integration -v ./internal/spoke/docker/
```

It exercises the full box lifecycle against a live daemon — create, guest
reachability, init-script provisioning, exec, dial, and destroy.

**End-to-end tests** live under [`e2e/`](../e2e/) behind the `e2e` build tag. They
run the real server (MCP tools + the admin/sign-in web UI) on a real HTTP
listener and drive the chatbot side over a real MCP client and the human side
through a real headless Chrome via
[WebDriver](https://github.com/tebeka/selenium). The external dependencies are
simulated (the box layer is an in-memory box manager), so the suite exercises box
creation over MCP, the admin dashboard, and the per-box proxy sign-in flow. A
separate cluster suite ([`e2e/cluster/`](../e2e/cluster)) exercises the
hub-and-spoke transport without a browser.

```bash
make test-e2e            # or: go test -tags e2e ./e2e/...
make test-e2e-cluster    # the hub-and-spoke clustering suite (no browser)
```

The browser e2e suite is opt-in: it only builds under `-tags e2e`, so the default
`go test ./...` unit run never includes it and stays fast. When you do run it and
no `chromedriver` is available (on `$CHROMEWEBDRIVER` or `$PATH`), it **fails**
rather than skipping — running the suite is an explicit request, so a missing
browser is an error, not a silent pass.

When `$LLMBOX_E2E_SCREENSHOT_DIR` is set, the tests also save PNG screenshots of
the proxy sign-in page (`signin-page.png` and the phone-sized
`signin-page-mobile.png`) to that directory. CI sets it
to [`.github/screenshots/`](../.github/screenshots) and, **on pull requests** (from
this repo), commits the refreshed images straight onto the PR branch and posts a
sticky comment previewing the changed images inline, so the
[proxy sign-in page](proxy.md#authentication) shown in the docs always matches the
live UI and the change is easy to review together with the code that caused it.
