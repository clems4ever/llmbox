package spoke

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/clems4ever/llmbox/internal/shared/cluster"
	"github.com/clems4ever/llmbox/internal/spoke/box"
)

// subcmd returns the named direct subcommand of cmd, failing the test if absent.
func subcmd(t *testing.T, cmd *cobra.Command, use string) *cobra.Command {
	t.Helper()
	for _, c := range cmd.Commands() {
		if c.Use == use {
			return c
		}
	}
	t.Fatalf("subcommand %q not found under %q", use, cmd.Use)
	return nil
}

// writeCAPEM writes a throwaway self-signed CA certificate to a temp file and
// returns its path, for exercising --tls-ca.
func writeCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	return path
}

// TestSpokeHubTLS checks hubTLS turns the --tls-ca / --tls-insecure flags into the
// right client config, and errors on an unreadable or non-cert CA file.
func TestSpokeHubTLS(t *testing.T) {
	// No flags: use the system trust store (nil config).
	if cfg, err := (spokeOptions{}).hubTLS(); err != nil || cfg != nil {
		t.Fatalf("default hubTLS = (%v, %v), want (nil, nil)", cfg, err)
	}
	// Insecure skip.
	cfg, err := (spokeOptions{tlsInsecure: true}).hubTLS()
	if err != nil || cfg == nil || !cfg.InsecureSkipVerify {
		t.Fatalf("insecure hubTLS = (%+v, %v), want InsecureSkipVerify set", cfg, err)
	}
	// A valid CA bundle populates RootCAs.
	cfg, err = (spokeOptions{tlsCAFile: writeCAPEM(t)}).hubTLS()
	if err != nil || cfg == nil || cfg.RootCAs == nil {
		t.Fatalf("CA hubTLS = (%+v, %v), want RootCAs set", cfg, err)
	}
	// A missing file and a non-cert file both error.
	if _, err := (spokeOptions{tlsCAFile: filepath.Join(t.TempDir(), "nope.pem")}).hubTLS(); err == nil {
		t.Error("missing --tls-ca file should error")
	}
	junk := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (spokeOptions{tlsCAFile: junk}).hubTLS(); err == nil {
		t.Error("a --tls-ca file with no certificates should error")
	}
}

// TestNewRootCmd checks the llmbox-spoke command wiring: the docker and firecracker
// backend subcommands each carry their own flags (never a mix).
func TestNewRootCmd(t *testing.T) {
	const name = "llmbox-spoke"
	cmd := NewRootCmd(name, "v0.1.0")
	if cmd.Use != name {
		t.Errorf("Use = %q, want %q", cmd.Use, name)
	}
	// The root command itself only dispatches to subcommands: it takes no run flags.
	if cmd.Flags().Lookup("hub") != nil {
		t.Error("root should carry no --hub flag; it lives on the backend subcommands")
	}

	// Flags every backend subcommand shares, plus the config-free invariant.
	common := []string{"hub", "token", "state", "tls-ca", "tls-insecure", "namespace", "init-script", "init-script-timeout", "copy", "box-memory-mb", "box-cpus", "box-pids-limit", "max-boxes", "box-socket-dir", "box-peer", "registry", "registry-username", "registry-password-file"}

	docker := subcmd(t, cmd, "docker")
	for _, f := range append([]string{"box-gpus", "image"}, common...) {
		if docker.Flags().Lookup(f) == nil {
			t.Errorf("docker subcommand missing --%s flag", f)
		}
	}
	// No firecracker flags and no --config leak onto the docker command.
	for _, f := range []string{"kernel", "rootfs", "payload", "state-dir", "disable-egress", "pool-size", "config", "backend"} {
		if docker.Flags().Lookup(f) != nil {
			t.Errorf("docker subcommand should not have --%s", f)
		}
	}

	fc := subcmd(t, cmd, "firecracker")
	for _, f := range append([]string{"kernel", "rootfs", "payload", "state-dir", "disable-egress", "pool-size"}, common...) {
		if fc.Flags().Lookup(f) == nil {
			t.Errorf("firecracker subcommand missing --%s flag", f)
		}
	}
	// No docker-only flag and no --config/--backend leak onto the firecracker command.
	for _, f := range []string{"box-gpus", "image", "config", "backend"} {
		if fc.Flags().Lookup(f) != nil {
			t.Errorf("firecracker subcommand should not have --%s", f)
		}
	}
}

// TestFirecrackerFetchCmd checks the firecracker `fetch` subcommand exposes the
// cache/registry flags and none of the spoke-run flags — it fetches images and
// exits rather than joining a hub.
func TestFirecrackerFetchCmd(t *testing.T) {
	cmd := NewRootCmd("llmbox-spoke", "v0.1.0")
	fc := subcmd(t, cmd, "firecracker")
	fetch := subcmd(t, fc, "fetch")

	for _, f := range []string{"state-dir", "registry", "registry-username", "registry-password-file"} {
		if fetch.Flags().Lookup(f) == nil {
			t.Errorf("fetch subcommand missing --%s flag", f)
		}
	}
	// A fetch-and-exit command carries no hub-join or per-box run flags.
	for _, f := range []string{"hub", "token", "kernel", "rootfs", "payload", "pool-size", "disable-egress"} {
		if fetch.Flags().Lookup(f) != nil {
			t.Errorf("fetch subcommand should not have --%s", f)
		}
	}
}

// TestFirecrackerVMCmd checks the firecracker `vm` operator command wires a `list`
// and a `destroy` subcommand, each carrying a --state-dir flag, and that `destroy`
// requires exactly one box argument.
func TestFirecrackerVMCmd(t *testing.T) {
	cmd := NewRootCmd("llmbox-spoke", "v0.1.0")
	fc := subcmd(t, cmd, "firecracker")
	vm := subcmd(t, fc, "vm")

	list := subcmd(t, vm, "list")
	if list.Flags().Lookup("state-dir") == nil {
		t.Error("vm list missing --state-dir flag")
	}
	destroy := subcmd(t, vm, "destroy <box-id|token>")
	if destroy.Flags().Lookup("state-dir") == nil {
		t.Error("vm destroy missing --state-dir flag")
	}
	if err := destroy.Args(destroy, nil); err == nil {
		t.Error("vm destroy accepted zero arguments; it needs one box id")
	}
	if err := destroy.Args(destroy, []string{"box"}); err != nil {
		t.Errorf("vm destroy rejected one argument: %v", err)
	}

	destroyAll := subcmd(t, vm, "destroy-all")
	for _, f := range []string{"state-dir", "yes"} {
		if destroyAll.Flags().Lookup(f) == nil {
			t.Errorf("vm destroy-all missing --%s flag", f)
		}
	}
}

// TestRunFirecrackerVMList checks the list output has a header row and a row per
// persisted box, with a dash for an absent box id and a state label.
func TestRunFirecrackerVMList(t *testing.T) {
	stateDir := t.TempDir()
	writeSpokeBoxMeta(t, stateDir, "aaaaaaaaaaaa", "box-a")
	writeSpokeBoxMeta(t, stateDir, "bbbbbbbbbbbb", "")

	var out strings.Builder
	if err := runFirecrackerVMList(&out, stateDir); err != nil {
		t.Fatalf("runFirecrackerVMList: %v", err)
	}
	s := out.String()
	for _, want := range []string{"TOKEN", "BOX ID", "STATE", "aaaaaaaaaaaa", "box-a", "bbbbbbbbbbbb"} {
		if !strings.Contains(s, want) {
			t.Errorf("list output missing %q\n%s", want, s)
		}
	}
	// A stopped box (no live VMM) is labelled stopped, and its missing box id shows a dash.
	if !strings.Contains(s, "stopped") {
		t.Errorf("list output missing a stopped state label\n%s", s)
	}
	if !strings.Contains(s, "-") {
		t.Errorf("list output missing a dash for the empty box id\n%s", s)
	}
}

// TestRunFirecrackerVMListEmpty checks the list command prints a friendly line, not
// a bare header, when the host has no boxes.
func TestRunFirecrackerVMListEmpty(t *testing.T) {
	var out strings.Builder
	if err := runFirecrackerVMList(&out, filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("runFirecrackerVMList: %v", err)
	}
	if !strings.Contains(out.String(), "no firecracker boxes") {
		t.Errorf("empty list output = %q, want a no-boxes message", out.String())
	}
}

// TestRunFirecrackerVMDestroy checks the destroy command removes the matched box and
// reports its token.
func TestRunFirecrackerVMDestroy(t *testing.T) {
	stateDir := t.TempDir()
	writeSpokeBoxMeta(t, stateDir, "aaaaaaaaaaaa", "box-a")

	var out strings.Builder
	if err := runFirecrackerVMDestroy(&out, stateDir, "box-a"); err != nil {
		t.Fatalf("runFirecrackerVMDestroy: %v", err)
	}
	if !strings.Contains(out.String(), "aaaaaaaaaaaa") {
		t.Errorf("destroy output = %q, want the destroyed token", out.String())
	}
	if _, err := os.Stat(filepath.Join(stateDir, "aaaaaaaaaaaa")); !os.IsNotExist(err) {
		t.Errorf("destroyed box dir still present: %v", err)
	}
}

// TestRunFirecrackerVMDestroyUnknown checks the destroy command surfaces an error
// when no box matches.
func TestRunFirecrackerVMDestroyUnknown(t *testing.T) {
	var out strings.Builder
	if err := runFirecrackerVMDestroy(&out, t.TempDir(), "nope"); err == nil {
		t.Fatal("runFirecrackerVMDestroy should error for an unknown box")
	}
}

// TestRunFirecrackerVMDestroyAll checks the destroy-all command removes every box and
// prints a per-box line plus a final count.
func TestRunFirecrackerVMDestroyAll(t *testing.T) {
	stateDir := t.TempDir()
	writeSpokeBoxMeta(t, stateDir, "aaaaaaaaaaaa", "box-a")
	writeSpokeBoxMeta(t, stateDir, "bbbbbbbbbbbb", "box-b")

	var out strings.Builder
	if err := runFirecrackerVMDestroyAll(&out, stateDir); err != nil {
		t.Fatalf("runFirecrackerVMDestroyAll: %v", err)
	}
	s := out.String()
	for _, want := range []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb", "destroyed 2 firecracker box(es)"} {
		if !strings.Contains(s, want) {
			t.Errorf("destroy-all output missing %q\n%s", want, s)
		}
	}
	for _, token := range []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb"} {
		if _, err := os.Stat(filepath.Join(stateDir, token)); !os.IsNotExist(err) {
			t.Errorf("box dir %s still present after destroy-all: %v", token, err)
		}
	}
}

// TestFirecrackerVMDestroyAllRequiresYes checks the destroy-all command refuses to
// run without --yes, so a stray invocation never wipes the host.
func TestFirecrackerVMDestroyAllRequiresYes(t *testing.T) {
	stateDir := t.TempDir()
	writeSpokeBoxMeta(t, stateDir, "aaaaaaaaaaaa", "box-a")

	cmd := NewRootCmd("llmbox-spoke", "v0.1.0")
	cmd.SetArgs([]string{"firecracker", "vm", "destroy-all", "--state-dir", stateDir})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("destroy-all without --yes should error")
	}
	// The box must be untouched when the guard trips.
	if _, err := os.Stat(filepath.Join(stateDir, "aaaaaaaaaaaa")); err != nil {
		t.Fatalf("destroy-all removed a box despite refusing without --yes: %v", err)
	}
}

// writeSpokeBoxMeta writes a minimal firecracker box meta.json under stateDir so the
// operator run functions have a box to list or destroy.
func writeSpokeBoxMeta(t *testing.T, stateDir, token, boxID string) {
	t.Helper()
	dir := filepath.Join(stateDir, token)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir box dir: %v", err)
	}
	meta := `{"token":"` + token + `","box_id":"` + boxID + `","phase":"ready","created":1}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}
}

// TestRunFirecrackerFetchBadRegistry checks the fetch command validates its
// registry credential before any download: a --registry with no password file is
// rejected up front.
func TestRunFirecrackerFetchBadRegistry(t *testing.T) {
	err := runFirecrackerFetch(context.Background(), "", registryFlags{host: "ghcr.io"})
	if err == nil {
		t.Fatal("runFirecrackerFetch should error when --registry is set without a password file")
	}
}

// TestDefaultSpokeStatePath checks the default credential location is the
// hidden .llmbox directory under the user's home, so the generated enrollment
// command needs no --state flag.
func TestDefaultSpokeStatePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no resolvable home directory in this environment")
	}
	want := filepath.Join(home, ".llmbox", "llmbox-spoke.json")
	if got := defaultSpokeStatePath(); got != want {
		t.Errorf("defaultSpokeStatePath() = %q, want %q", got, want)
	}
}

// TestSpokeCredsRoundTrip checks saved spoke credentials round-trip through the
// state file and that a missing file reads back as nil.
func TestSpokeCredsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spoke.json")

	// Missing file is not an error; returns nil.
	if c, err := loadSpokeCreds(path); err != nil || c != nil {
		t.Fatalf("loadSpokeCreds(missing) = (%v,%v)", c, err)
	}

	want := cluster.Credentials{Name: "edge", Credential: "secret"}
	if err := saveSpokeCreds(path, want); err != nil {
		t.Fatalf("saveSpokeCreds: %v", err)
	}
	got, err := loadSpokeCreds(path)
	if err != nil || got == nil || *got != want {
		t.Fatalf("loadSpokeCreds = (%+v,%v), want %+v", got, err, want)
	}
}

// TestCheckStateWritable checks the pre-enrollment probe accepts a writable state
// directory and rejects a read-only one, so a non-writable state location fails
// fast instead of burning the one-time join token on an unsavable enrollment.
func TestCheckStateWritable(t *testing.T) {
	// Writable: a fresh temp dir, and a nested path whose parent does not exist yet.
	dir := t.TempDir()
	if err := checkStateWritable(filepath.Join(dir, "spoke.json")); err != nil {
		t.Fatalf("checkStateWritable(writable) = %v, want nil", err)
	}
	if err := checkStateWritable(filepath.Join(dir, "sub", "spoke.json")); err != nil {
		t.Fatalf("checkStateWritable(creatable subdir) = %v, want nil", err)
	}

	// Read-only: a 0500 directory the probe cannot create a file in. (Skipped when
	// the test runs as root, which bypasses the permission bits.)
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permissions")
	}
	ro := filepath.Join(dir, "readonly")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatalf("mkdir read-only: %v", err)
	}
	if err := checkStateWritable(filepath.Join(ro, "spoke.json")); err == nil {
		t.Fatal("checkStateWritable(read-only) = nil, want a not-writable error")
	}
}

// TestRunSpokeRequiresTokenOrCreds checks runSpoke errors when neither a join
// token nor saved credentials are available for enrollment.
func TestRunSpokeRequiresTokenOrCreds(t *testing.T) {
	o := spokeOptions{
		hubURL:    "wss://hub/spoke/connect",
		statePath: filepath.Join(t.TempDir(), "none.json"),
	}
	err := runSpoke(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("runSpoke err = %v, want a token-required error", err)
	}
}

// TestRunSpokeRejectsBadGPUs checks runSpoke fails fast on a malformed --box-gpus
// spec, before attempting any enrollment.
func TestRunSpokeRejectsBadGPUs(t *testing.T) {
	o := spokeOptions{
		hubURL:    "wss://hub/spoke/connect",
		token:     "tok",
		statePath: filepath.Join(t.TempDir(), "none.json"),
		boxGPUs:   "0",
	}
	err := runSpoke(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "box-gpus") {
		t.Fatalf("runSpoke err = %v, want a box-gpus error", err)
	}
}

// TestRunSpokeRejectsBadInitScript checks --init-script is validated up front: a
// path that cannot be read fails the spoke before it does any hub work.
func TestRunSpokeRejectsBadInitScript(t *testing.T) {
	o := spokeOptions{
		hubURL:         "wss://hub/spoke/connect",
		token:          "tok",
		statePath:      filepath.Join(t.TempDir(), "none.json"),
		initScriptPath: filepath.Join(t.TempDir(), "does-not-exist.sh"),
	}
	err := runSpoke(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "init-script") {
		t.Fatalf("runSpoke err = %v, want an init-script error", err)
	}
}

// TestSpokeInitScriptFromFlag checks --init-script reads the file's bytes and that
// an unset flag yields no script.
func TestSpokeInitScriptFromFlag(t *testing.T) {
	// Unset: no script, no error.
	if s, err := (spokeOptions{}).initScript(); err != nil || s != nil {
		t.Fatalf("initScript(unset) = (%q, %v), want (nil, nil)", s, err)
	}

	path := filepath.Join(t.TempDir(), "provision.sh")
	body := "#!/bin/sh\napt-get install -y jq\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := (spokeOptions{initScriptPath: path}).initScript()
	if err != nil {
		t.Fatalf("initScript: %v", err)
	}
	if string(s) != body {
		t.Fatalf("initScript = %q, want %q", s, body)
	}
}

// TestSpokeParsePublishPorts checks --publish-port parses the bare-port and
// port:description forms, and rejects malformed, out-of-range, and duplicate
// values so a typo fails the spoke at startup rather than on every box create.
func TestSpokeParsePublishPorts(t *testing.T) {
	// Unset: no ports, no error.
	if p, err := (spokeOptions{}).parsePublishPorts(); err != nil || p != nil {
		t.Fatalf("parsePublishPorts(unset) = (%v, %v), want (nil, nil)", p, err)
	}

	got, err := (spokeOptions{publishPorts: []string{"8080", "3000:vite dev server"}}).parsePublishPorts()
	if err != nil {
		t.Fatalf("parsePublishPorts: %v", err)
	}
	if len(got) != 2 ||
		got[0].Port != 8080 || got[0].Description != "" ||
		got[1].Port != 3000 || got[1].Description != "vite dev server" {
		t.Fatalf("parsed = %+v, want [{8080 } {3000 vite dev server}]", got)
	}

	for _, bad := range []string{"", "abc", "0", "70000", "-1", "99999", "8x:desc"} {
		if _, err := (spokeOptions{publishPorts: []string{bad}}).parsePublishPorts(); err == nil {
			t.Errorf("parsePublishPorts(%q) = nil error, want error", bad)
		}
	}
	if _, err := (spokeOptions{publishPorts: []string{"8080", "8080:again"}}).parsePublishPorts(); err == nil {
		t.Error("parsePublishPorts(duplicate) = nil error, want error")
	}
}

// TestSpokeInitScriptErrors checks --init-script rejects a missing file and an
// empty (whitespace-only) script, so a misconfigured provisioning file fails the
// spoke at startup rather than silently on every box create.
func TestSpokeInitScriptErrors(t *testing.T) {
	if _, err := (spokeOptions{initScriptPath: filepath.Join(t.TempDir(), "nope.sh")}).initScript(); err == nil {
		t.Error("initScript(missing file) = nil error, want error")
	}

	empty := filepath.Join(t.TempDir(), "empty.sh")
	if err := os.WriteFile(empty, []byte("   \n\t\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := (spokeOptions{initScriptPath: empty}).initScript(); err == nil {
		t.Error("initScript(empty file) = nil error, want error")
	}
}

// TestSpokeCopyFiles checks --copy resolves a single file and a directory tree to
// copy metadata: the host and box paths, each file's preserved mode, an explicit
// destination honoured and an omitted one defaulted to the source's absolute path,
// skipped symlinks, and nil when the flag is unset. Content is not read here — the
// manager streams it at box-create time — so only metadata is asserted.
func TestSpokeCopyFiles(t *testing.T) {
	// Unset: nothing to copy, no error.
	if f, err := (spokeOptions{}).copyFiles(); err != nil || f != nil {
		t.Fatalf("copyFiles(unset) = (%v, %v), want (nil, nil)", f, err)
	}

	dir := t.TempDir()
	// A standalone file copied to an explicit box destination.
	single := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(single, []byte("k: v\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// A directory tree with a nested regular file and an executable, plus a symlink
	// that must be skipped.
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "sub", "data.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data.txt", filepath.Join(tree, "sub", "link")); err != nil {
		t.Fatal(err)
	}

	got, err := (spokeOptions{copyPaths: []string{
		single + ":/home/agent/config.yaml",
		tree + ":/opt/tree",
	}}).copyFiles()
	if err != nil {
		t.Fatalf("copyFiles: %v", err)
	}

	byBoxPath := map[string]box.CopyFile{}
	for _, f := range got {
		byBoxPath[f.BoxPath] = f
	}
	if len(got) != 3 {
		t.Fatalf("copied %d files, want 3 (symlink skipped): %v", len(got), byBoxPath)
	}
	if f, ok := byBoxPath["/home/agent/config.yaml"]; !ok || f.HostPath != single || f.Mode != 0o640 {
		t.Errorf("single = %+v, want host %q mode 0640", f, single)
	}
	if f, ok := byBoxPath["/opt/tree/sub/data.txt"]; !ok || f.Mode != 0o644 {
		t.Errorf("nested = %+v, want mode 0644", f)
	}
	if f, ok := byBoxPath["/opt/tree/run.sh"]; !ok || f.Mode != 0o755 {
		t.Errorf("executable = %+v, want mode 0755", f)
	}

	// No explicit destination: it defaults to the source's absolute path.
	deflt, err := (spokeOptions{copyPaths: []string{single}}).copyFiles()
	if err != nil {
		t.Fatalf("copyFiles(default dest): %v", err)
	}
	if len(deflt) != 1 || deflt[0].BoxPath != single {
		t.Fatalf("default-dest copy = %+v, want one file at %q", deflt, single)
	}
}

// TestSpokeCopyFilesErrors checks --copy fails the spoke at startup on a relative
// destination, an empty source, and a missing source, rather than failing silently
// on every box create.
func TestSpokeCopyFilesErrors(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]string{
		"relative dest":  f + ":relative/dest",
		"empty source":   ":/dest",
		"missing source": filepath.Join(t.TempDir(), "nope") + ":/dest",
	} {
		if _, err := (spokeOptions{copyPaths: []string{spec}}).copyFiles(); err == nil {
			t.Errorf("copyFiles(%s: %q) = nil error, want error", name, spec)
		}
	}
}

// TestSpokeRegistriesFromFlags checks the registry flags resolve to a credential
// with the password read from its file, and that a missing host means none while
// a missing password file is an error.
func TestSpokeRegistriesFromFlags(t *testing.T) {
	// No registry host: anonymous pulls, no entry.
	if regs, err := (spokeOptions{}).registries(); err != nil || regs != nil {
		t.Fatalf("registries(no host) = (%v, %v), want (nil, nil)", regs, err)
	}

	// Host with a password file: one resolved entry, password trimmed from file.
	pw := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(pw, []byte("ghp_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := spokeOptions{registry: registryFlags{host: "ghcr.io", username: "bob", passwordFile: pw}}
	regs, err := o.registries()
	if err != nil {
		t.Fatalf("registries: %v", err)
	}
	if len(regs) != 1 || regs[0].Registry != "ghcr.io" || regs[0].Username != "bob" || regs[0].Password != "ghp_secret" {
		t.Errorf("registries = %+v, want one ghcr.io/bob entry with trimmed password", regs)
	}

	// Host without a password file: error.
	if _, err := (spokeOptions{registry: registryFlags{host: "ghcr.io"}}).registries(); err == nil {
		t.Error("registries(host, no password file) = nil error, want error")
	}
}
