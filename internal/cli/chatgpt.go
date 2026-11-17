package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/roast/internal/auth/chatgpt"
	"github.com/bluesky-social/roast/internal/textsafe"
)

func newChatGPTCommand(stdout io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "chatgpt",
		Short: "Manage Roast's independent ChatGPT plan connection",
	}
	root.AddCommand(
		newChatGPTSignInCommand(stdout),
		newChatGPTStatusCommand(stdout),
		newChatGPTSelectCommand(),
		newChatGPTSignOutCommand(stdout),
		newChatGPTModelsCommand(stdout),
		newChatGPTPrepareHostCommand(stdout),
	)
	return root
}

func newChatGPTSignInCommand(stdout io.Writer) *cobra.Command {
	var account string
	var enablePlan bool
	var diagnostics bool
	signin := &cobra.Command{
		Use:     "signin",
		Aliases: []string{"login"},
		Short:   "Continue with ChatGPT (authorize plan access in your browser)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := chatgpt.New()
			if err != nil {
				return err
			}
			if diagnostics {
				client.SetSignInDiagnostics(cmd.ErrOrStderr())
			}
			signIn := client.SignIn
			if enablePlan {
				signIn = client.EnablePlan
			}
			err = signIn(cmd.Context(), account, func(authURL string) error {
				_, err := fmt.Fprintf(stdout, "Continue with ChatGPT\nOpen this URL in a browser on this computer:\n%s\n", authURL)
				return err
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, "ChatGPT account connected. Review usage and permissions at https://chatgpt.com/settings/usage")
			return nil
		},
	}
	signin.Flags().BoolVar(&diagnostics, "diagnostics", false, "Report fixed sign-in phases without authorization data")
	signin.Flags().BoolVar(&enablePlan, "enable-plan", false, "Explicitly request consent to enable plan usage for --account")
	signin.Flags().StringVar(&account, "account", "", "Reauthorize an existing issued client ID instead of registering a new account")
	return signin
}

func newChatGPTStatusCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "List saved accounts and the active registration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := chatgpt.New()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			accounts, err := client.Status(ctx)
			if err != nil {
				return err
			}
			if len(accounts) == 0 {
				fmt.Fprintln(stdout, "No ChatGPT accounts. Run roast chatgpt signin.")
			}
			for _, account := range accounts {
				fmt.Fprintf(stdout, "%s  %s  active=%t signed_in=%t plan_enabled=%t pending_registration=%t\n",
					textsafe.Text(account.ClientID), textsafe.Text(account.Email),
					account.Active, account.SignedIn, account.PlanEnabled, account.PendingRegistration)
			}
			return nil
		},
	}
}

func newChatGPTSelectCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "select CLIENT_ID",
		Short: "Select a saved ChatGPT registration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := chatgpt.New()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			return client.Select(ctx, args[0])
		},
	}
}

func newChatGPTSignOutCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "signout [CLIENT_ID]",
		Short: "Revoke and clear this registration's tokens, retaining its identity",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := chatgpt.New()
			if err != nil {
				return err
			}
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			if err := client.SignOut(ctx, id); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "Signed out of ChatGPT.")
			return nil
		},
	}
}

func newChatGPTModelsCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "models",
		Short: "List models available to the selected ChatGPT account",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := chatgpt.New()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			models, err := client.Models(ctx)
			if err != nil {
				return err
			}
			for _, model := range models {
				if model.Visibility == "list" {
					fmt.Fprintf(stdout, "%s\t%s\n", textsafe.Text(model.Slug), textsafe.Text(model.DisplayName))
				}
			}
			return nil
		},
	}
}

func newChatGPTPrepareHostCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "prepare-host",
		Short: "Create a stable host identity before a user-managed VM credential transfer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := chatgpt.New()
			if err != nil {
				return err
			}
			id, dir, err := client.PrepareHost(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Host ID: %s\nProtected credential directory: %s\nPreserve this host ID when transferring a registration over a secure channel.\n", textsafe.Text(id), textsafe.Text(dir))
			return nil
		},
	}
}
