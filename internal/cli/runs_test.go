package cli

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/record"
)

func TestRunsSyncUploadsExistingBundleWithoutEnablingAutomation(t *testing.T) {
	recordDir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", recordDir)
	t.Setenv("SYNC_UPLOAD_ACCESS", "access")
	t.Setenv("SYNC_UPLOAD_SECRET", "secret")
	runID := "20260920T120000Z-1234abcd"
	runDir := filepath.Join(recordDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, record.MetaFile), []byte(`{"run_id":"`+runID+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var uploaded bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		if r.Method != http.MethodPut || !strings.HasPrefix(r.URL.Path, "/runs-bucket/manual/sha256/") {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		uploaded = true
		w.Header().Set("ETag", `"uploaded"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configBody := fmt.Sprintf(`upload:
  endpoint: %s
  bucket: runs-bucket
  prefix: manual
  region: us-east-1
  access_key_env: SYNC_UPLOAD_ACCESS
  secret_key_env: SYNC_UPLOAD_SECRET
  path_style: true
`, server.URL)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"runs", "sync", "--config", configPath, "--include-existing"}, &stdout, io.Discard)
	if err != nil || code != ExitClean || !uploaded || !strings.Contains(stdout.String(), "uploaded") {
		t.Fatalf("code=%d err=%v uploaded=%v output=%s", code, err, uploaded, stdout.String())
	}
}

// TestRunsShowEscapesStoredReports: a report written before render escaped
// finding text can hold raw control bytes; showing it must not replay them.
func TestRunsShowEscapesStoredReports(t *testing.T) {
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	runID := "20261031T000000Z-0123abcd"
	if _, err := record.WriteReportRequired(runID, "### R-1 [HIGH] x\x1b]0;pwned\x07\x1b[2J\n- **Reasoning:** \u009b31m\n"); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"runs", "show", runID}, &stdout, &bytes.Buffer{})
	if err != nil || code != ExitClean {
		t.Fatalf("code=%d err=%v", code, err)
	}
	out := stdout.String()
	if strings.ContainsAny(out, "\x1b\x07\u009b") {
		t.Errorf("runs show replayed control bytes: %q", out)
	}
	if !strings.Contains(out, `\x1b]0;pwned\x07`) {
		t.Errorf("escapes must stay visible: %q", out)
	}
}
