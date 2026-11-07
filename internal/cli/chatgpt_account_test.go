package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/chatgpt"
	"github.com/bluesky-social/roast/internal/testutil"
)

type accountRoundTripper func(*http.Request) (*http.Response, error)

func (f accountRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Only synthetic, unexpired tokens in an isolated store are used. Replacing
// the whole transport makes any unexpected request fail instead of reaching a
// real auth or inference service. These tests must not run in parallel.
func mockAccountTransport(t *testing.T, transport accountRoundTripper) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func accountFixture(t *testing.T) (configPath, accountDir string, selectAccount func(string) error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("direct ChatGPT auth is not supported on Windows")
	}
	configPath = isolateUserConfig(t)
	t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
	t.Setenv("ROAST_NO_HOOK", "1")
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	accountDir = filepath.Join(filepath.Dir(configPath), "chatgpt")
	if err := os.MkdirAll(accountDir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, value any) {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(accountDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("accounts.json", map[string]any{"active": "account-a", "clients": []string{"account-a", "account-b"}})
	for _, account := range []string{"account-a", "account-b"} {
		write(fmt.Sprintf("%x.json", sha256.Sum256([]byte(account))), map[string]any{
			"client_id": account, "subject": account,
			"access_token": "synthetic-" + account,
			"scope":        "resource.invoke chatgpt.tokens.use.direct",
			"expires_at":   time.Now().Add(time.Hour),
		})
	}
	selector, err := chatgpt.New()
	if err != nil {
		t.Fatal(err)
	}
	return configPath, accountDir, func(account string) error { return selector.Select(t.Context(), account) }
}

func writeAccountConfig(t *testing.T, path, finder, verifier string) {
	t.Helper()
	var body strings.Builder
	body.WriteString("version: 1\ninference_profiles:\n")
	for i, role := range []string{"finder", "verifier"} {
		protocol := []string{finder, verifier}[i]
		fmt.Fprintf(&body, "  %s:\n    model: %s-model\n    protocol: %s\n    max_output_tokens: 1024\n", role, role, protocol)
		if protocol != "chatgpt" {
			body.WriteString("    endpoint: https://provider.invalid/v1\n    api_key: synthetic-api-key\n")
		}
	}
	body.WriteString("roles:\n  finder:\n    profile: finder\n  verifier:\n    profile: verifier\nupload:\n  enabled: false\n")
	if err := os.WriteFile(path, []byte(body.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

func accountToolResponse(model, tool, args string, direct bool) (*http.Response, error) {
	item := map[string]any{
		"id": "fc-test", "type": "function_call", "call_id": "call-test", "status": "completed",
		"name": tool, "arguments": args,
	}
	if direct {
		item["namespace"] = "roast"
	}
	w := httptest.NewRecorder()
	err := testutil.WriteInferenceStream(w, "responses", map[string]any{
		"id": "resp-test", "object": "response", "created_at": 1, "status": "completed", "model": model,
		"output": []any{item},
		"usage": map[string]any{
			"input_tokens": 10, "output_tokens": 3, "total_tokens": 13,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		},
	})
	return w.Result(), err
}

func TestReviewPinsChatGPTAccountAcrossRoles(t *testing.T) {
	for _, protocols := range [][2]string{{"chatgpt", "chatgpt"}, {"responses", "chatgpt"}, {"chatgpt", "responses"}} {
		t.Run(strings.Join(protocols[:], "-"), func(t *testing.T) {
			configPath, _, selectAccount := accountFixture(t)
			writeAccountConfig(t, configPath, protocols[0], protocols[1])
			t.Chdir(initChangedRepo(t))
			var mu sync.Mutex
			var sent []string
			mockAccountTransport(t, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
					return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				var request struct{ Model string }
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					return nil, err
				}
				mu.Lock()
				sent = append(sent, request.Model+":"+r.Header.Get("Authorization"))
				mu.Unlock()
				tool := "submit_verdict"
				args := `{"reasoning":"the panic is unconditional","status":"confirmed","confidence":0.99,"grounding":"bad.go line 3 calls panic unconditionally","recalibrated_severity":""}`
				if request.Model == "finder-model" {
					// This models another process selecting B while finder A runs.
					if err := selectAccount("account-b"); err != nil {
						return nil, err
					}
					tool = "findings"
					args = `{"assessment":"FINDINGS_PRESENT","findings":[{"reasoning":"panic is reachable","severity":"HIGH","confidence":0.95,"cwe":"CWE-248","description":"The function always panics.","file":"bad.go","lines":[3],"code_quote":"panic(\"oops\")","fixed_code":"return","frontend_category":""}]}`
				}
				return accountToolResponse(request.Model, tool, args, r.URL.Host == "api.openai.com")
			})
			for _, account := range []string{"account-a", "account-b"} {
				code, err := ExecuteContext(t.Context(), []string{
					"review", "--config", configPath, "--format", "json", "--uncommitted-only", "--max-attempts=1",
				}, io.Discard, io.Discard)
				if err != nil || code != ExitClean {
					t.Fatalf("operation starting with %s: code=%d err=%v", account, code, err)
				}
			}
			var want []string
			for _, account := range []string{"account-a", "account-b"} {
				for i, role := range []string{"finder", "verifier"} {
					token := "synthetic-api-key"
					if protocols[i] == "chatgpt" {
						token = "synthetic-" + account
					}
					want = append(want, role+"-model:Bearer "+token)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(sent, want) {
				t.Fatalf("requests = %v, want %v", sent, want)
			}
		})
	}
}

func TestDoctorPinsChatGPTAccountAcrossChecks(t *testing.T) {
	for _, finderProtocol := range []string{"chatgpt", "responses"} {
		t.Run(finderProtocol+"-finder", func(t *testing.T) {
			configPath, _, selectAccount := accountFixture(t)
			writeAccountConfig(t, configPath, finderProtocol, "chatgpt")
			var accounts []string
			var methods []string
			mockAccountTransport(t, func(r *http.Request) (*http.Response, error) {
				direct := r.URL.Host == "api.openai.com"
				if direct {
					accounts = append(accounts, r.Header.Get("Authorization"))
					methods = append(methods, r.Method)
				}
				// Switching at the first catalog check also tests that the
				// following roundtrip and later role reuse the same snapshot.
				if err := selectAccount("account-b"); err != nil {
					return nil, err
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"finder-model"},{"slug":"verifier-model"}]}`))}, nil
				case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					var request struct{ Model string }
					if err := json.Unmarshal(body, &request); err != nil {
						return nil, err
					}
					tool, args := "doctor_lookup", `{}`
					if token := doctorTokenPattern.FindString(string(body)); token != "" {
						tool, args = "doctor_probe", fmt.Sprintf(`{"ok":true,"token":%q}`, token)
					}
					return accountToolResponse(request.Model, tool, args, direct)
				default:
					return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
			})
			for _, account := range []string{"account-a", "account-b"} {
				accounts, methods = nil, nil
				results := doctorChecks(t.Context(), configPath)
				for _, result := range results {
					if result.Status == doctorFail {
						t.Errorf("operation starting with %s: %#v", account, result)
					}
				}
				wantMethods := []string{"GET", "POST", "POST"}
				if finderProtocol == "chatgpt" {
					wantMethods = append(wantMethods, wantMethods...)
				}
				if !reflect.DeepEqual(methods, wantMethods) {
					t.Fatalf("direct requests = %v, want %v", methods, wantMethods)
				}
				for _, got := range accounts {
					if want := "Bearer synthetic-" + account; got != want {
						t.Errorf("account = %q, want %q", got, want)
					}
				}
			}
		})
	}
}

func TestReviewKeepsUnusedChatGPTVerifierLazy(t *testing.T) {
	configPath, accountDir, _ := accountFixture(t)
	writeAccountConfig(t, configPath, "responses", "chatgpt")
	if err := os.WriteFile(filepath.Join(accountDir, "accounts.json"), []byte("invalid index"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(initChangedRepo(t))
	requests := 0
	mockAccountTransport(t, func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "provider.invalid" {
			return nil, fmt.Errorf("unexpected request to %s", r.URL)
		}
		return accountToolResponse("finder-model", "findings", `{"assessment":"NO_FINDINGS","findings":[]}`, false)
	})
	code, err := ExecuteContext(t.Context(), []string{
		"review", "--config", configPath, "--format", "json", "--uncommitted-only", "--max-attempts=1",
	}, io.Discard, io.Discard)
	if err != nil || code != ExitClean || requests != 1 {
		t.Fatalf("code=%d err=%v requests=%d", code, err, requests)
	}
}
