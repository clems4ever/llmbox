package apikey

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
)

// defaultAPIKeyTTL is how long a new API key stays valid when --ttl is not
// given: one year, long enough for a deployed integration without minting an
// eternal credential.
const defaultAPIKeyTTL = 365 * 24 * time.Hour

// apiKeyIDLen is how many leading hash characters the CLI shows (and accepts as
// an --id prefix) for an API key — enough to be unambiguous in practice without
// dumping the full hash.
const apiKeyIDLen = 12

// NewCmd builds the `apikey` command tree (add/list/delete) that manages the API
// keys authenticating box-control API callers. Every subcommand operates
// directly on the hub's SQLite state file, so these commands run on the hub
// host.
//
// @return *cobra.Command The configured `apikey` command with its add/list/delete subcommands.
//
// @testcase TestNewCmd checks the add/list/delete subcommands and their flags are registered.
func NewCmd() *cobra.Command {
	apikeyCmd := &cobra.Command{
		Use:   "apikey",
		Short: "Manage box-control API keys",
		Args:  cobra.NoArgs,
	}

	var (
		addStateFile string
		addName      string
		addTTL       time.Duration
	)
	addCmd := &cobra.Command{
		Use:           "add",
		Short:         "Mint a new API key",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if addName == "" {
				return errors.New("--name is required (a label identifying what the key is for)")
			}
			return addAPIKey(cmd.OutOrStdout(), addStateFile, addName, addTTL)
		},
	}
	addCmd.Flags().StringVar(&addStateFile, "state-file", config.DefaultStateFile, "the hub's state file the key is written to (must match the running hub's state_file)")
	addCmd.Flags().StringVar(&addName, "name", "", "label identifying what the key is for (e.g. ci, mcp-prod)")
	addCmd.Flags().DurationVar(&addTTL, "ttl", defaultAPIKeyTTL, "how long the key stays valid")
	apikeyCmd.AddCommand(addCmd)

	var listStateFile string
	listCmd := &cobra.Command{
		Use:           "list",
		Short:         "List API keys",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return listAPIKeys(cmd.OutOrStdout(), listStateFile, time.Now())
		},
	}
	listCmd.Flags().StringVar(&listStateFile, "state-file", config.DefaultStateFile, "the hub's state file to read keys from")
	apikeyCmd.AddCommand(listCmd)

	var (
		deleteStateFile string
		deleteID        string
		deleteName      string
	)
	deleteCmd := &cobra.Command{
		Use:           "delete",
		Short:         "Delete API keys by ID or name",
		SilenceUsage:  true,
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if deleteID == "" && deleteName == "" {
				return errors.New("one of --id or --name is required")
			}
			return deleteAPIKeys(cmd.OutOrStdout(), deleteStateFile, deleteID, deleteName)
		},
	}
	deleteCmd.Flags().StringVar(&deleteStateFile, "state-file", config.DefaultStateFile, "the hub's state file to delete keys from")
	deleteCmd.Flags().StringVar(&deleteID, "id", "", "delete the single key whose ID has this prefix")
	deleteCmd.Flags().StringVar(&deleteName, "name", "", "delete every key with this name")
	apikeyCmd.AddCommand(deleteCmd)

	return apikeyCmd
}

// addAPIKey opens the hub's store, mints a new API key, and prints the secret
// once to out.
//
// @arg out The writer the key is printed to.
// @arg stateFile The hub's state file holding the API key store.
// @arg name The label identifying what the key is for.
// @arg ttl How long the key stays valid.
// @error error if the store cannot be opened or the key cannot be minted.
//
// @testcase TestAddAPIKeyCmdPrintsKey mints a key and prints it once.
func addAPIKey(out io.Writer, stateFile, name string, ttl time.Duration) error {
	st, err := storepkg.Open(stateFile)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	secret, err := Create(st, name, ttl, time.Now())
	if err != nil {
		return err
	}
	// Show the state file the key landed in: a key is only honored by a hub
	// reading this exact same store, so a mismatch here (e.g. the wrong
	// --state-file) is the usual cause of "invalid API key".
	fmt.Fprintf(out, "API key %q (valid %s):\n\n  %s\n\nWritten to state file: %s\n(the running hub must use this same state_file, or it will reject the key)\n\nPass it as a bearer token, e.g.:\n\n  curl -H \"Authorization: Bearer %s\" ...\n", name, ttl, secret, stateFile, secret)
	return nil
}

// listAPIKeys prints the stored API keys (short ID, name, creation, and
// expiry/expired marker) from the hub's store.
//
// @arg out The writer the listing is printed to.
// @arg stateFile The hub's state file holding the API key store.
// @arg now The current time, used to flag expired keys.
// @error error if the store cannot be opened or read.
//
// @testcase TestListAPIKeysCmd lists keys with their name and expiry.
func listAPIKeys(out io.Writer, stateFile string, now time.Time) error {
	st, err := storepkg.Open(stateFile)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	keys, err := st.ListAPIKeys()
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Fprintln(out, "No API keys.")
		return nil
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
	fmt.Fprintf(out, "%-*s  %-20s  %-20s  %s\n", apiKeyIDLen, "ID", "NAME", "CREATED", "EXPIRES")
	for _, k := range keys {
		status := k.ExpiresAt.Format(time.RFC3339)
		if now.After(k.ExpiresAt) {
			status += " (expired)"
		}
		fmt.Fprintf(out, "%-*s  %-20s  %-20s  %s\n", apiKeyIDLen, shortID(k.ID), k.Name, k.CreatedAt.Format("2006-01-02 15:04"), status)
	}
	return nil
}

// deleteAPIKeys deletes API keys by ID prefix or by name. With idPrefix set it
// deletes the single key whose ID starts with it (erroring if none or more than
// one match); with name set it deletes every key with that name.
//
// @arg out The writer deletion results are printed to.
// @arg stateFile The hub's state file holding the API key store.
// @arg idPrefix The ID prefix selecting a single key; empty to select by name.
// @arg name The key name selecting all its keys; empty to select by ID.
// @error error if the store cannot be opened, no key matches, or an ID prefix is ambiguous.
//
// @testcase TestDeleteAPIKeyByID deletes the single key matching an ID prefix.
// @testcase TestDeleteAPIKeyByName deletes every key with a name.
// @testcase TestDeleteAPIKeyNoMatch errors when nothing matches.
func deleteAPIKeys(out io.Writer, stateFile, idPrefix, name string) error {
	st, err := storepkg.Open(stateFile)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	keys, err := st.ListAPIKeys()
	if err != nil {
		return err
	}

	var matched []storepkg.APIKeyInfo
	for _, k := range keys {
		if idPrefix != "" && strings.HasPrefix(k.ID, idPrefix) {
			matched = append(matched, k)
		} else if name != "" && k.Name == name {
			matched = append(matched, k)
		}
	}
	if len(matched) == 0 {
		return errors.New("no api key matches")
	}
	if idPrefix != "" && len(matched) > 1 {
		return fmt.Errorf("ID prefix %q is ambiguous (%d keys match); use more characters", idPrefix, len(matched))
	}
	for _, k := range matched {
		if err := st.DeleteAPIKey(k.ID); err != nil {
			return err
		}
		fmt.Fprintf(out, "deleted api key %s (%s)\n", shortID(k.ID), k.Name)
	}
	return nil
}

// shortID returns the leading apiKeyIDLen characters of an API key hash ID for
// display; a shorter ID is returned unchanged.
//
// @arg id The full hash ID.
// @return string The display prefix of the ID.
//
// @testcase TestListAPIKeysCmd shows keys by their short ID.
func shortID(id string) string {
	if len(id) <= apiKeyIDLen {
		return id
	}
	return id[:apiKeyIDLen]
}
