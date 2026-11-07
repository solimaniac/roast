package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/roast/internal/buildinfo"
	"github.com/bluesky-social/roast/internal/chatgpt"
	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/gate"
	"github.com/bluesky-social/roast/internal/llm"
	"github.com/bluesky-social/roast/internal/scope"
	"github.com/bluesky-social/roast/internal/skill"
	"github.com/bluesky-social/roast/internal/updatecheck"
)

type doctorStatus string

const (
	doctorPass doctorStatus = "pass"
	doctorWarn doctorStatus = "warn"
	doctorFail doctorStatus = "fail"
)

type doctorResult struct {
	Name   string       `json:"name"`
	Status doctorStatus `json:"status"`
	Detail string       `json:"detail"`
}

func newDoctorCommand(stdout io.Writer) *cobra.Command {
	var configPath string
	var asJSON bool
	cmd := &cobra.Command{
		Use:           "doctor",
		Short:         "Check configuration, providers, and local integration",
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			results := doctorChecks(cmd.Context(), configPath)
			if asJSON {
				encoder := json.NewEncoder(stdout)
				encoder.SetIndent("", "  ")
				if err := encoder.Encode(results); err != nil {
					return fmt.Errorf("write doctor results: %w", err)
				}
			} else {
				for _, result := range results {
					fmt.Fprintf(stdout, "%-5s %-18s %s\n", strings.ToUpper(string(result.Status)), result.Name, result.Detail)
				}
			}
			for _, result := range results {
				if result.Status == doctorFail {
					return fmt.Errorf("one or more doctor checks failed")
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file (default: user config directory/roast/config.yaml)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a JSON array")
	return cmd
}

func doctorChecks(ctx context.Context, configPath string) []doctorResult {
	results := []doctorResult{doctorBuildCheck()}
	path := configPath
	if path == "" {
		var err error
		path, err = config.UserPath()
		if err != nil {
			results = append(results, doctorResult{"config", doctorFail, err.Error()})
			return append(results, doctorLocalChecks(ctx)...)
		}
	}
	if _, err := os.Stat(path); err == nil {
		results = append(results, doctorResult{"config-file", doctorPass, path})
	} else if os.IsNotExist(err) && configPath == "" {
		results = append(results, doctorResult{"config-file", doctorWarn, "not found; using defaults and environment: " + path})
	} else if err != nil {
		results = append(results, doctorResult{"config-file", doctorFail, err.Error()})
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		results = append(results, doctorResult{"config", doctorFail, err.Error()})
		return append(results, doctorLocalChecks(ctx)...)
	}
	results = append(results, doctorResult{"config", doctorPass, fmt.Sprintf("schema version %d", cfg.Version)})

	client := &http.Client{Timeout: 5 * time.Second}
	for _, name := range []string{"finder", "verifier"} {
		role, resolveErr := cfg.ResolveRole(name)
		if resolveErr != nil {
			results = append(results, doctorResult{name + "-profile", doctorFail, resolveErr.Error()})
			continue
		}
		if name == "finder" {
			flip, flipErr := cfg.ResolveFinderFlip(role, config.AuthorModel())
			if flipErr != nil {
				results = append(results, doctorResult{name + "-profile", doctorFail, flipErr.Error()})
				continue
			}
			role = flip.Role
		}
		credential := doctorResult{name + "-credential", doctorPass, role.Credential.String()}
		if role.Protocol == "codex" {
			loginContext, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := llm.CheckCodexLogin(loginContext)
			cancel()
			if err != nil {
				credential.Status, credential.Detail = doctorFail, err.Error()
			} else {
				credential.Detail = "Codex CLI reports ChatGPT login"
			}
		} else if role.Protocol == "chatgpt" {
			credentialContext, cancel := context.WithTimeout(ctx, 30*time.Second)
			auth, err := chatgpt.New()
			if err == nil {
				_, err = auth.Token(credentialContext)
			}
			cancel()
			if err != nil {
				credential.Status, credential.Detail = doctorFail, err.Error()
			} else {
				credential.Detail = "Roast-managed ChatGPT account has a current plan-usage grant"
			}
		} else if strings.TrimSpace(role.APIKey) == "" {
			credential.Status = doctorFail
		}
		results = append(results, credential)
		if validateErr := config.ValidateRole(name, role); validateErr != nil {
			results = append(results, doctorResult{name + "-profile", doctorFail, validateErr.Error()})
			continue
		}
		detail := fmt.Sprintf("%s: %s via %s at %s with credential %s", role.Profile, role.Model, role.Protocol, role.BaseURL, role.Credential)
		if role.SessionHeader != "" {
			detail += ", session header " + role.SessionHeader
		}
		results = append(results, doctorResult{Name: name + "-profile", Status: doctorPass, Detail: detail})
		if credential.Status == doctorFail {
			continue
		}
		results = append(results, doctorEndpointCheck(ctx, client, name, role))
		results = append(results, doctorRoundtripCheck(ctx, name, role))
	}

	if !cfg.Upload.Enabled {
		results = append(results, doctorResult{"uploads", doctorPass, "disabled"})
	} else if upload, resolveErr := cfg.ResolveUpload(); resolveErr != nil {
		results = append(results, doctorResult{"uploads", doctorFail, resolveErr.Error()})
	} else {
		results = append(results, doctorResult{"uploads", doctorPass, fmt.Sprintf("%s backend at %s", upload.Backend, upload.Endpoint)})
	}
	return append(results, doctorLocalChecks(ctx)...)
}

func doctorRoundtripCheck(ctx context.Context, name string, role config.ResolvedRole) doctorResult {
	maxOutputTokens := role.MaxOutputTokens
	if maxOutputTokens <= 0 || maxOutputTokens > 512 {
		maxOutputTokens = 512
	}
	// Allow Codex CLI startup and ChatGPT generation time for both turns.
	requestTimeout, probeTimeout := 15*time.Second, 35*time.Second
	if role.Protocol == "codex" || role.Protocol == "chatgpt" {
		requestTimeout, probeTimeout = 120*time.Second, 250*time.Second
	}
	probeContext, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	client, err := llm.New(probeContext, llm.ClientConfig{
		Role: name, BaseURL: role.BaseURL, APIKey: role.APIKey, Model: role.Model,
		Protocol: role.Protocol, MaxOutputTokens: maxOutputTokens, SessionHeader: role.SessionHeader,
		MaxAttempts: 1, RequestTimeout: requestTimeout,
	})
	if err != nil {
		return doctorResult{name + "-roundtrip", doctorFail, err.Error()}
	}
	// Exercise what a review relies on: a tool call, its result replayed on the
	// next request, and a final submission made on the reserved wrap-up turn,
	// all under the same automatic tool choice. A single-call probe passes on
	// routes that then fail every review.
	token := "roast-doctor-" + rand.Text()
	looked := false
	lookup := llm.Tool{
		Name: "doctor_lookup", Description: "Return the diagnostic token.",
		Schema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		Run: func(context.Context, map[string]any) (any, error) {
			looked = true
			return map[string]any{"token": token}, nil
		},
	}
	final := llm.Tool{
		Name: "doctor_probe", Description: "Return the diagnostic result.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ok":    map[string]any{"type": "boolean"},
				"token": map[string]any{"type": "string"},
			},
			"required": []string{"ok", "token"}, "additionalProperties": false,
		},
	}
	var output struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
	}
	err = client.Converse(probeContext,
		"This is a connectivity check. Call doctor_lookup once, then call doctor_probe with ok=true and the token doctor_lookup returned.",
		"Run the check now.",
		llm.ConverseOpts{Tools: []llm.Tool{lookup, final}, FinalTool: final.Name, MaxTurns: 2}, &output)
	if err != nil {
		if errors.Is(err, llm.ErrAuth) {
			return doctorResult{name + "-roundtrip", doctorFail, "credential rejected by the configured inference route"}
		}
		return doctorResult{name + "-roundtrip", doctorFail, fmt.Sprintf("%s/%s two-turn tool probe failed: %v", role.Protocol, role.Model, err)}
	}
	if !output.OK {
		return doctorResult{name + "-roundtrip", doctorFail, "doctor_probe returned ok=false"}
	}
	if !looked || output.Token != token {
		return doctorResult{name + "-roundtrip", doctorFail, "doctor_probe did not return the token from doctor_lookup; the tool result did not round-trip"}
	}
	return doctorResult{name + "-roundtrip", doctorPass, fmt.Sprintf("%s via %s (two-turn tool loop)", role.Model, role.Protocol)}
}

func doctorBuildCheck() doctorResult {
	if buildinfo.Version == "dev" {
		return doctorResult{"build", doctorWarn, buildinfo.String() + "; release check disabled"}
	}
	return doctorResult{"build", doctorPass, buildinfo.String()}
}

func doctorEndpointCheck(ctx context.Context, client *http.Client, name string, role config.ResolvedRole) doctorResult {
	if role.Protocol == "chatgpt" {
		catalogContext, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		auth, err := chatgpt.New()
		if err != nil {
			return doctorResult{name + "-endpoint", doctorFail, err.Error()}
		}
		models, err := auth.Models(catalogContext)
		if err != nil {
			return doctorResult{name + "-endpoint", doctorFail, err.Error()}
		}
		for _, model := range models {
			if model.Slug == role.Model {
				return doctorResult{name + "-endpoint", doctorPass, "selected model is in this ChatGPT account's current catalog"}
			}
		}
		return doctorResult{name + "-endpoint", doctorFail, "configured model is not in this ChatGPT account's current catalog; run roast chatgpt models"}
	}
	if role.Protocol == "codex" {
		return doctorResult{name + "-endpoint", doctorPass, "codex://chatgpt (CLI-managed service; no HTTP model listing)"}
	}
	endpoint := strings.TrimRight(role.BaseURL, "/") + "/models"
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return doctorResult{name + "-endpoint", doctorFail, "invalid models endpoint: " + err.Error()}
	}
	requestContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint, nil)
	if err != nil {
		return doctorResult{name + "-endpoint", doctorFail, err.Error()}
	}
	if role.Protocol == "anthropic" {
		req.Header.Set("x-api-key", role.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+role.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return doctorResult{name + "-endpoint", doctorFail, fmt.Sprintf("GET %s: %v", endpoint, err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return doctorResult{name + "-endpoint", doctorPass, fmt.Sprintf("GET %s: HTTP %s", endpoint, resp.Status)}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return doctorResult{name + "-endpoint", doctorFail, fmt.Sprintf("credential rejected by %s: HTTP %s", endpoint, resp.Status)}
	case resp.StatusCode >= 500:
		return doctorResult{name + "-endpoint", doctorFail, fmt.Sprintf("%s returned HTTP %s", endpoint, resp.Status)}
	default:
		return doctorResult{name + "-endpoint", doctorWarn, fmt.Sprintf("endpoint is reachable but model listing returned HTTP %s", resp.Status)}
	}
}

func doctorLocalChecks(ctx context.Context) []doctorResult {
	results := doctorGitChecks()
	results = append(results, doctorSkillChecks()...)
	if updateCheckDisabled() {
		return append(results, doctorResult{"release", doctorWarn, "disabled by ROAST_NO_UPDATE_CHECK"})
	}
	checker, err := updatecheck.Default()
	if err != nil {
		results = append(results, doctorResult{"release", doctorWarn, err.Error()})
		return results
	}
	status := checker.Check(ctx, buildinfo.Version, true)
	switch {
	case status.Skipped:
		results = append(results, doctorResult{"release", doctorWarn, "development build; release check skipped"})
	case status.Error != "":
		results = append(results, doctorResult{"release", doctorWarn, status.Error})
	case status.UpdateAvailable():
		results = append(results, doctorResult{"release", doctorWarn, fmt.Sprintf("%s available; running %s", status.Latest, status.Current)})
	default:
		results = append(results, doctorResult{"release", doctorPass, fmt.Sprintf("%s is current", status.Current)})
	}
	return results
}

func doctorGitChecks() []doctorResult {
	repo, err := scope.GitRoot(".")
	if err != nil {
		return []doctorResult{{"git", doctorWarn, "not inside a Git repository"}}
	}
	results := []doctorResult{{"git", doctorPass, repo}}
	status, err := gate.Inspect(repo)
	if err != nil {
		return append(results, doctorResult{"git-gate", doctorWarn, err.Error()})
	}
	if status.Stale || (status.Decision == gate.DecisionEnabled && !status.Active()) {
		return append(results, doctorResult{"git-gate", doctorWarn, status.Summary()})
	}
	return append(results, doctorResult{"git-gate", doctorPass, status.Summary()})
}

func doctorSkillChecks() []doctorResult {
	targets, err := skill.InstallationTargets("")
	if err != nil {
		return []doctorResult{{"agent-skills", doctorWarn, err.Error()}}
	}
	results := make([]doctorResult, 0, len(targets))
	for _, target := range targets {
		name := target.Harness + "-skill"
		status, inspectErr := skill.Inspect(target.Path)
		if inspectErr != nil {
			results = append(results, doctorResult{name, doctorWarn, inspectErr.Error()})
			continue
		}
		switch status.State {
		case skill.NotInstalled:
			results = append(results, doctorResult{name, doctorWarn, "not installed; run `roast skill install`"})
		case skill.Stale:
			results = append(results, doctorResult{name, doctorWarn, "stale; run `roast skill install --force`"})
		case skill.Symlink:
			results = append(results, doctorResult{name, doctorPass, target.Path + " (development symlink)"})
		default:
			results = append(results, doctorResult{name, doctorPass, target.Path})
		}
	}
	return results
}
