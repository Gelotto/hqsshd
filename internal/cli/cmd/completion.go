package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

var completionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish]",
	Short: "Generate shell completion scripts",
	Long: `Generate shell completion scripts for hqssh.

Bash:
  $ source <(hqssh completion bash)
  # To load completions for each session:
  $ hqssh completion bash > /etc/bash_completion.d/hqssh

Zsh:
  $ source <(hqssh completion zsh)
  # To load completions for each session:
  $ hqssh completion zsh > "${fpath[1]}/_hqssh"

Fish:
  $ hqssh completion fish | source
  # To load completions for each session:
  $ hqssh completion fish > ~/.config/fish/completions/hqssh.fish`,
	DisableFlagsInUseLine: true,
	ValidArgs:             []string{"bash", "zsh", "fish"},
	Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return rootCmd.GenBashCompletionV2(os.Stdout, true)
		case "zsh":
			return rootCmd.GenZshCompletion(os.Stdout)
		case "fish":
			return rootCmd.GenFishCompletion(os.Stdout, true)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(completionCmd)

	// Add dynamic completion for commands that take IDs
	attachCmd.ValidArgsFunction = completeSessionIDs
	if killCmd := findSubcommand("kill"); killCmd != nil {
		killCmd.ValidArgsFunction = completeSessionIDs
	}
	runCmd.ValidArgsFunction = completeTaskIDs
	cancelCmd.ValidArgsFunction = completeRunIDs
}

// completeSessionIDs provides dynamic session ID completion.
func completeSessionIDs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp // Prefix completion handled by shell
}

// completeTaskIDs provides dynamic task ID completion.
func completeTaskIDs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// completeRunIDs provides dynamic run ID completion.
func completeRunIDs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// findSubcommand finds a subcommand by name on rootCmd.
func findSubcommand(name string) *cobra.Command {
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == name {
			return cmd
		}
	}
	return nil
}
