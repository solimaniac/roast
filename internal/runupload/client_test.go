package runupload

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/testutil"
)

func TestS3CompatibleStoreUsesExplicitCredentialsAndContentAddressedKey(t *testing.T) {
	body := []byte("bundle bytes")
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/runs-bucket/team/roast/sha256/abc123.tar.zst" {
			http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusBadRequest)
			return
		}
		if auth := r.Header.Get("Authorization"); !strings.Contains(auth, "Credential=explicit-access/") || !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
			http.Error(w, "request was not signed with the explicit access key", http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("X-Amz-Meta-Roast-Bundle-Sha256"); got != "abc123" {
			http.Error(w, "missing bundle digest metadata", http.StatusBadRequest)
			return
		}
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("ETag", `"etag-value"`)
		w.Header().Set("x-amz-version-id", "version-1")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store, err := NewStore(config.ResolvedUpload{
		Endpoint: server.URL, Bucket: "runs-bucket", Prefix: "team/roast", Region: "us-east-1",
		AccessKey: "explicit-access", SecretKey: "explicit-secret", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Put(t.Context(), "run-1", bytes.NewReader(body), int64(len(body)), "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("uploaded body = %q", gotBody)
	}
	if result.Key != "team/roast/sha256/abc123.tar.zst" || result.ETag != "etag-value" || result.VersionID != "version-1" {
		t.Fatalf("upload result = %#v", result)
	}
}

func TestPresignedStoreUsesBearerOnlyForControlPlane(t *testing.T) {
	body := []byte("bundle bytes")
	bundlePath := filepath.Join(t.TempDir(), "bundle.tar.zst")
	if err := os.WriteFile(bundlePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := os.Open(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bundle.Close() })
	var dataAuthorization string
	data := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dataAuthorization = r.Header.Get("Authorization")
		if r.Header.Get("X-Bound") != "yes" {
			http.Error(w, "missing bound header", http.StatusBadRequest)
			return
		}
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("x-amz-version-id", "v1")
		w.WriteHeader(http.StatusOK)
	}))
	defer data.Close()
	var complete bool
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer broker-secret" {
			http.Error(w, "bad bearer", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/roast/runs":
			_ = json.NewEncoder(w).Encode(uploadGrant{
				RunID: "run-1", AttemptID: "attempt", ObjectKey: "runs/run-1/bundle.tar.zst",
				Method: http.MethodPut, UploadURL: data.URL, RequiredHeaders: map[string]string{"X-Bound": "yes"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/roast/runs/run-1/complete":
			complete = true
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "uploaded"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer control.Close()
	store, err := NewStore(config.ResolvedUpload{Backend: "presigned", Endpoint: control.URL, Token: "broker-secret"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Put(t.Context(), "run-1", bundle, int64(len(body)), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.Stat(); err != nil {
		t.Fatalf("HTTP closed the caller-owned bundle file: %v", err)
	}
	if !complete || dataAuthorization != "" || result.Key != "runs/run-1/bundle.tar.zst" || result.VersionID != "v1" {
		t.Fatalf("complete=%v data auth=%q result=%#v", complete, dataAuthorization, result)
	}
}

// grantingBroker returns a broker that issues uploadURL for every run.
func grantingBroker(t *testing.T, uploadURL string) string {
	t.Helper()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/roast/runs" {
			_ = json.NewEncoder(w).Encode(uploadGrant{
				RunID: "run-1", AttemptID: "attempt", ObjectKey: "runs/run-1/bundle.tar.zst",
				Method: http.MethodPut, UploadURL: uploadURL,
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(control.Close)
	return control.URL
}

func TestPresignedUploadFailureDoesNotRecordSignedQuery(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	signedURL := closed.URL + "/bucket/run.tar.zst?X-Amz-Credential=cred&X-Amz-Signature=signature-secret#frag"
	closed.Close()
	store, err := NewStore(config.ResolvedUpload{Backend: "presigned", Endpoint: grantingBroker(t, signedURL), Token: "broker-secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, putErr := store.Put(t.Context(), "run-1", strings.NewReader("bundle"), 6, strings.Repeat("a", 64))
	if putErr == nil || strings.Contains(putErr.Error(), "signature-secret") || strings.Contains(putErr.Error(), "X-Amz") || !strings.Contains(putErr.Error(), "/bucket/run.tar.zst") {
		t.Fatalf("put error = %v", putErr)
	}

	recordDir := t.TempDir()
	if err := EnsureEnrolled(recordDir); err != nil {
		t.Fatal(err)
	}
	runID := "20260920T120000Z-1234abcd"
	makeRun(t, recordDir, runID)
	outcome := uploadRun(t.Context(), store, recordDir, runID, false)
	state, err := readState(recordDir, runID)
	if err != nil || state == nil || outcome.Result != ResultTransientFailed {
		t.Fatalf("outcome=%#v state=%#v err=%v", outcome, state, err)
	}
	summaries, err := ListStatus(recordDir)
	if err != nil || len(summaries) != 1 {
		t.Fatalf("summaries=%#v err=%v", summaries, err)
	}
	for name, text := range map[string]string{"last_error": state.LastError, "sync outcome": outcome.Detail, "status": summaries[0].Detail} {
		if strings.Contains(text, "signature-secret") || strings.Contains(text, "X-Amz") {
			t.Errorf("%s exposed the signed query: %s", name, text)
		}
	}
}

func TestPresignedUploadURLMustUseHTTPS(t *testing.T) {
	for name, test := range map[string]struct {
		uploadURL     string
		allowInsecure bool
		accepted      bool
	}{
		"https":                       {"https://objects.example.test/run", false, true},
		"loopback http":               {"http://127.0.0.1:9/run", false, true},
		"localhost http":              {"http://localhost:9/run", false, true},
		"remote http":                 {"http://objects.example.test/run", false, false},
		"remote http with opt-in":     {"http://objects.example.test/run", true, true},
		"unsupported scheme":          {"file:///etc/passwd", true, false},
		"relative URL":                {"/run", true, false},
		"remote http with userinfo":   {"http://user@objects.example.test/run", false, false},
		"uppercase loopback hostname": {"http://LOCALHOST:9/run", false, true},
	} {
		t.Run(name, func(t *testing.T) {
			store := &presignedStore{allowInsecure: test.allowInsecure}
			if err := store.checkUploadURL(test.uploadURL); (err == nil) != test.accepted {
				t.Fatalf("checkUploadURL(%q) = %v, accepted want %v", test.uploadURL, err, test.accepted)
			}
		})
	}

	remote := "http://objects.example.test/bucket/run?X-Amz-Signature=signature-secret"
	store, err := NewStore(config.ResolvedUpload{Backend: "presigned", Endpoint: grantingBroker(t, remote), Token: "broker-secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, putErr := store.Put(t.Context(), "run-1", strings.NewReader("bundle"), 6, strings.Repeat("a", 64))
	if putErr == nil || !strings.Contains(putErr.Error(), "must use https") || strings.Contains(putErr.Error(), "signature-secret") {
		t.Fatalf("plaintext grant error = %v", putErr)
	}
}

func TestEnrollmentCreatesOwnerOnlyRecordRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	recordDir := filepath.Join(t.TempDir(), "records")
	if err := EnsureEnrolled(recordDir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(recordDir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("record root mode = %v, err=%v", info.Mode().Perm(), err)
	}
}

func TestMain(m *testing.M) {
	testutil.ClearAmbientEnvironment()
	os.Exit(m.Run())
}
