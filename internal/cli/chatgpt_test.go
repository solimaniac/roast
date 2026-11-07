package cli

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestChatGPTCommands(t *testing.T) {
	var out bytes.Buffer
	cmd := newChatGPTCommand(&out)
	for _, name := range []string{"signin", "login", "status", "select", "signout", "models"} {
		found, _, err := cmd.Find([]string{name})
		if err != nil || found == cmd {
			t.Fatalf("missing command %s", name)
		}
	}
	cmd.SetArgs([]string{"select"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("select requires explicit client ID")
	}
}
func TestChatGPTEmptyStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("direct ChatGPT auth is not supported on Windows")
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	var out bytes.Buffer
	cmd := newChatGPTCommand(&out)
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No ChatGPT accounts") {
		t.Fatalf("unexpected status: %s", out.String())
	}
}
