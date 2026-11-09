package chatgpt

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

func assertCallbackCompletePage(t *testing.T, header http.Header, body string) {
	t.Helper()
	const wantBody = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Authorization received</title>
<script>history.replaceState(null,'','/auth/complete');</script>
</head>
<body><p>Authorization received. Return to Roast to check the result.</p></body>
</html>
`
	if body != wantBody {
		t.Fatal("callback completion page changed or included request data")
	}
	_, scriptAndRest, ok := strings.Cut(body, "<script>")
	if !ok {
		t.Fatal("callback URL cleanup script missing")
	}
	script, _, ok := strings.Cut(scriptAndRest, "</script>")
	if !ok || script != "history.replaceState(null,'','/auth/complete');" {
		t.Fatal("callback script does not replace the URL with the fixed completion path")
	}
	hash := sha256.Sum256([]byte(script))
	policy := "default-src 'none'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(hash[:]) + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	for name, want := range map[string]string{
		"Content-Type":            "text/html; charset=utf-8",
		"Content-Security-Policy": policy,
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
	} {
		if got := header.Values(name); len(got) != 1 || got[0] != want {
			t.Errorf("unexpected callback %s header", name)
		}
	}
	if header.Get("Location") != "" || header.Get("Set-Cookie") != "" {
		t.Fatal("callback completion page redirects or sets cookies")
	}
}
