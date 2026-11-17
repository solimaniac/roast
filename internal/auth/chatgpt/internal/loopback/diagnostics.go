package loopback

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
)

// Diagnostics emits fixed, value-free sign-in events.
type Diagnostics struct {
	write func(string)
}

func NewDiagnostics(w io.Writer) Diagnostics {
	if w == nil {
		return Diagnostics{}
	}
	var mu sync.Mutex
	return Diagnostics{write: func(event string) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = fmt.Fprintln(w, "chatgpt_signin:"+event)
	}}
}

func (diagnostic Diagnostics) Emit(event string) {
	// Keep this allowlist at the output boundary: even an accidental dynamic
	// argument must never expose authorization data.
	switch event {
	case "listener_ready", "listener_failed", "connection_opened", "connection_active", "connection_closed", "rejected_method", "rejected_path", "rejected_host", "rejected_state", "rejected_duplicate", "callback_received", "callback_failed", "exchange_started", "canceled", "timed_out":
		if diagnostic.write != nil {
			diagnostic.write(event)
		}
	}
}

func callbackConnState(diagnostic Diagnostics) func(net.Conn, http.ConnState) {
	if diagnostic.write == nil {
		return nil
	}
	// Capture the configured emitter: a final close hook can run after Shutdown
	// returns. Never inspect the connection, its addresses, or its payload.
	return func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			diagnostic.Emit("connection_opened")
		case http.StateActive:
			// Active means request bytes were read, not that HTTP was valid or
			// that the callback handler ran.
			diagnostic.Emit("connection_active")
		case http.StateClosed:
			diagnostic.Emit("connection_closed")
		}
	}
}
