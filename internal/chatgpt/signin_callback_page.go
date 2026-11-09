package chatgpt

import (
	"io"
	"net/http"
)

// This page never includes callback data or reports sign-in success: token
// exchange and validation finish separately. Its only script replaces the
// callback URL in history without navigating or sending another request.
const callbackCompletePage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Authorization received</title>
<script>history.replaceState(null,'','/auth/complete');</script>
</head>
<body><p>Authorization received. Return to Roast to check the result.</p></body>
</html>
`

// Keep this hash in sync with the exact static script bytes above. No other
// scripts, resources, frames, forms, or base URLs are allowed.
const callbackCompletePolicy = "default-src 'none'; script-src 'sha256-eAngspPXv2k/ukGQyHLbfpsuKrAbDQbH5ffw2WzwDBo='; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func writeCallbackComplete(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", callbackCompletePolicy)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, callbackCompletePage)
}
