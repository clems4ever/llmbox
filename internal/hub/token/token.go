// Package token implements the llmbox-server `token` command tree — creating,
// listing, and revoking the one-time join tokens a hub issues to enroll spokes.
// The commands operate directly on the hub's SQLite state file, so they run on
// the hub host (not on a spoke). The cmd/llmbox-server binary mounts NewCmd under
// its root command.
package token

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/clems4ever/llmbox/internal/hub/config"
	storepkg "github.com/clems4ever/llmbox/internal/hub/store"
	"github.com/clems4ever/llmbox/internal/shared/cluster"
)

// defaultJoinTokenTTL is how long a generated join token stays valid when
// --ttl is not given.
const defaultJoinTokenTTL = time.Hour

// joinTokenIDLen is how many leading hash characters the CLI shows (and accepts
// as an --id prefix) for a join token — enough to be unambiguous in practice
// without dumping the full hash.
const joinTokenIDLen = 12

// NewCmd builds the `token` command tree (create/list/revoke) that manages the
// one-time join tokens a hub issues to enroll spokes. Every subcommand operates
// directly on the hub's SQLite state file, so these commands run on the hub host.
//
// @return *cobra.Command The configured `token` command with its create/list/revoke subcommands.
//
// @testcase TestNewCmd checks the create/list/revoke subcommands and their flags are registered.
func NewCmd() *cobra.Command {
	tokenCmd := &cobra.Command{
		Use:   "token",
		Short: "Manage spoke join tokens",
		Args:  cobra.NoArgs,
	}

	var (
		stateFile string
		spokeName string
		ttl       time.Duration
	)
	createCmd := &cobra.Command{
		Use:           "create",
		Short:         "Mint a one-time join token for a named spoke",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if spokeName == "" {
				return errors.New("--name is required (the spoke name baked into the token)")
			}
			return createJoinToken(cmd.OutOrStdout(), stateFile, spokeName, ttl)
		},
	}
	createCmd.Flags().StringVar(&stateFile, "state-file", config.DefaultStateFile, "the hub's state file the token is written to (must match the running hub's state_file)")
	createCmd.Flags().StringVar(&spokeName, "name", "", "name of the spoke this token enrolls")
	createCmd.Flags().DurationVar(&ttl, "ttl", defaultJoinTokenTTL, "how long the token stays valid")
	tokenCmd.AddCommand(createCmd)

	var listStateFile string
	listCmd := &cobra.Command{
		Use:           "list",
		Short:         "List outstanding spoke join tokens",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return listJoinTokens(cmd.OutOrStdout(), listStateFile, time.Now())
		},
	}
	listCmd.Flags().StringVar(&listStateFile, "state-file", config.DefaultStateFile, "the hub's state file to read tokens from")
	tokenCmd.AddCommand(listCmd)

	var (
		revokeStateFile string
		revokeID        string
		revokeName      string
	)
	revokeCmd := &cobra.Command{
		Use:           "revoke",
		Short:         "Revoke spoke join tokens by ID or spoke name",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if revokeID == "" && revokeName == "" {
				return errors.New("one of --id or --name is required")
			}
			return revokeJoinTokens(cmd.OutOrStdout(), revokeStateFile, revokeID, revokeName)
		},
	}
	revokeCmd.Flags().StringVar(&revokeStateFile, "state-file", config.DefaultStateFile, "the hub's state file to revoke tokens from")
	revokeCmd.Flags().StringVar(&revokeID, "id", "", "revoke the single token whose ID has this prefix")
	revokeCmd.Flags().StringVar(&revokeName, "name", "", "revoke every token issued for this spoke name")
	tokenCmd.AddCommand(revokeCmd)

	return tokenCmd
}

// createJoinToken opens the hub's store, mints a one-time join token for the
// named spoke, and prints it once to out.
//
// @arg out The writer the token is printed to.
// @arg stateFile The hub's state file holding the cluster store.
// @arg spokeName The spoke name baked into the token.
// @arg ttl How long the token stays valid.
// @error error if the store cannot be opened or the token cannot be minted.
//
// @testcase TestCreateJoinTokenCmdPrintsToken mints a token and prints it once.
func createJoinToken(out io.Writer, stateFile, spokeName string, ttl time.Duration) error {
	store, err := storepkg.Open(stateFile)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	token, err := cluster.CreateJoinToken(store, spokeName, ttl, time.Now())
	if err != nil {
		return err
	}
	// Show the state file the token landed in: a token is only honored by a hub
	// reading this exact same store, so a mismatch here (e.g. the wrong
	// --state-file) is the usual cause of "enrollment rejected".
	fmt.Fprintf(out, "Join token for spoke %q (valid %s, one-time use):\n\n  %s\n\nWritten to state file: %s\n(the running hub must use this same state_file, or it will reject the token)\n\nStart the spoke with:\n\n  llmbox-spoke docker --hub wss://<hub>/spoke/connect --token %s\n", spokeName, ttl, token, stateFile, token)
	return nil
}

// listJoinTokens prints the outstanding join tokens (short ID, spoke name, and
// expiry/expired marker) from the hub's store.
//
// @arg out The writer the listing is printed to.
// @arg stateFile The hub's state file holding the cluster store.
// @arg now The current time, used to flag expired tokens.
// @error error if the store cannot be opened or read.
//
// @testcase TestListJoinTokensCmd lists outstanding tokens with their spoke and expiry.
func listJoinTokens(out io.Writer, stateFile string, now time.Time) error {
	store, err := storepkg.Open(stateFile)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	tokens, err := store.ListJoinTokens()
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		fmt.Fprintln(out, "No outstanding join tokens.")
		return nil
	}
	sort.Slice(tokens, func(i, j int) bool { return tokens[i].Name < tokens[j].Name })
	fmt.Fprintf(out, "%-*s  %-20s  %s\n", joinTokenIDLen, "ID", "SPOKE", "EXPIRES")
	for _, t := range tokens {
		status := t.ExpiresAt.Format(time.RFC3339)
		if now.After(t.ExpiresAt) {
			status += " (expired)"
		}
		fmt.Fprintf(out, "%-*s  %-20s  %s\n", joinTokenIDLen, shortID(t.ID), t.Name, status)
	}
	return nil
}

// revokeJoinTokens deletes join tokens by ID prefix or by spoke name. With idPrefix
// set it revokes the single token whose ID starts with it (erroring if none or
// more than one match); with name set it revokes every token for that spoke.
//
// @arg out The writer revocation results are printed to.
// @arg stateFile The hub's state file holding the cluster store.
// @arg idPrefix The ID prefix selecting a single token; empty to select by name.
// @arg name The spoke name selecting all its tokens; empty to select by ID.
// @error error if the store cannot be opened, no token matches, or an ID prefix is ambiguous.
//
// @testcase TestRevokeJoinTokenByID revokes the single token matching an ID prefix.
// @testcase TestRevokeJoinTokenByName revokes every token for a spoke name.
// @testcase TestRevokeJoinTokenNoMatch errors when nothing matches.
func revokeJoinTokens(out io.Writer, stateFile, idPrefix, name string) error {
	store, err := storepkg.Open(stateFile)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	tokens, err := store.ListJoinTokens()
	if err != nil {
		return err
	}

	var matched []cluster.JoinTokenInfo
	for _, t := range tokens {
		if idPrefix != "" && strings.HasPrefix(t.ID, idPrefix) {
			matched = append(matched, t)
		} else if name != "" && t.Name == name {
			matched = append(matched, t)
		}
	}
	if len(matched) == 0 {
		return errors.New("no join token matches")
	}
	if idPrefix != "" && len(matched) > 1 {
		return fmt.Errorf("ID prefix %q is ambiguous (%d tokens match); use more characters", idPrefix, len(matched))
	}
	for _, t := range matched {
		if err := store.DeleteJoinToken(t.ID); err != nil {
			return err
		}
		fmt.Fprintf(out, "Revoked join token %s for spoke %q.\n", shortID(t.ID), t.Name)
	}
	return nil
}

// shortID truncates a join token hash ID to the display length.
//
// @arg id The full hash ID.
// @return string The leading joinTokenIDLen characters (or the whole id if shorter).
//
// @testcase TestListJoinTokensCmd shows shortened token IDs.
func shortID(id string) string {
	if len(id) <= joinTokenIDLen {
		return id
	}
	return id[:joinTokenIDLen]
}
