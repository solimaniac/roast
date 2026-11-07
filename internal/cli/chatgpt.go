package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/bluesky-social/roast/internal/chatgpt"
	"github.com/bluesky-social/roast/internal/textsafe"
	"github.com/spf13/cobra"
)

func newChatGPTCommand(stdout io.Writer) *cobra.Command {
	root := &cobra.Command{Use: "chatgpt", Short: "Manage Roast's independent ChatGPT plan connection"}
	var account string
	var enablePlan bool
	signin := &cobra.Command{Use: "signin", Aliases: []string{"login"}, Short: "Continue with ChatGPT (authorize plan access in your browser)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, e := chatgpt.New()
		if e != nil {
			return e
		}
		signin := c.SignIn
		if enablePlan {
			signin = c.EnablePlan
		}
		e = signin(cmd.Context(), account, func(u string) error {
			_, e := fmt.Fprintf(stdout, "Continue with ChatGPT\nOpen this URL in a browser on this computer:\n%s\n", u)
			return e
		})
		if e == nil {
			fmt.Fprintln(stdout, "ChatGPT account connected. Review usage and permissions at https://chatgpt.com/settings/usage")
		}
		return e
	}}
	signin.Flags().BoolVar(&enablePlan, "enable-plan", false, "Explicitly request consent to enable plan usage for --account")
	signin.Flags().StringVar(&account, "account", "", "Reauthorize an existing issued client ID instead of registering a new account")
	status := &cobra.Command{Use: "status", Short: "List saved accounts and the active registration", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, e := chatgpt.New()
		if e != nil {
			return e
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()
		accounts, e := c.Status(ctx)
		if e != nil {
			return e
		}
		if len(accounts) == 0 {
			fmt.Fprintln(stdout, "No ChatGPT accounts. Run roast chatgpt signin.")
		}
		for _, a := range accounts {
			fmt.Fprintf(stdout, "%s  %s  active=%t signed_in=%t plan_enabled=%t pending_registration=%t\n", textsafe.Text(a.ClientID), textsafe.Text(a.Email), a.Active, a.SignedIn, a.PlanEnabled, a.PendingRegistration)
		}
		return nil
	}}
	selectCmd := &cobra.Command{Use: "select CLIENT_ID", Short: "Select a saved ChatGPT registration", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, e := chatgpt.New()
		if e != nil {
			return e
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		defer cancel()
		return c.Select(ctx, args[0])
	}}
	signout := &cobra.Command{Use: "signout [CLIENT_ID]", Short: "Revoke and clear this registration's tokens, retaining its identity", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, e := chatgpt.New()
		if e != nil {
			return e
		}
		id := ""
		if len(args) > 0 {
			id = args[0]
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		if e = c.SignOut(ctx, id); e != nil {
			return e
		}
		fmt.Fprintln(stdout, "Signed out of ChatGPT.")
		return nil
	}}
	models := &cobra.Command{Use: "models", Short: "List models available to the selected ChatGPT account", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, e := chatgpt.New()
		if e != nil {
			return e
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		models, e := c.Models(ctx)
		if e != nil {
			return e
		}
		for _, m := range models {
			if m.Visibility == "list" {
				fmt.Fprintf(stdout, "%s\t%s\n", textsafe.Text(m.Slug), textsafe.Text(m.DisplayName))
			}
		}
		return nil
	}}
	prepare := &cobra.Command{Use: "prepare-host", Short: "Create a stable host identity before a user-managed VM credential transfer", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, e := chatgpt.New()
		if e != nil {
			return e
		}
		id, dir, e := c.PrepareHost(cmd.Context())
		if e != nil {
			return e
		}
		fmt.Fprintf(stdout, "Host ID: %s\nProtected credential directory: %s\nPreserve this host ID when transferring a registration over a secure channel.\n", textsafe.Text(id), textsafe.Text(dir))
		return nil
	}}
	root.AddCommand(signin, status, selectCmd, signout, models, prepare)
	return root
}
