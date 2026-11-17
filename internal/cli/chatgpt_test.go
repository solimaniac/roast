package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestChatGPTCommands(t *testing.T) {
	for _, name := range []string{"signin", "login", "status", "select", "signout", "models", "prepare-host"} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := newChatGPTCommand(&out)
			found, _, err := cmd.Find([]string{name})
			if err != nil || found == cmd {
				t.Fatalf("Find(%q) = %v, %v", name, found, err)
			}
			want := name
			if name == "login" {
				want = "signin"
			}
			if found.Name() != want {
				t.Errorf("command = %q, want %q", found.Name(), want)
			}
		})
	}
}

func TestChatGPTCommandRejectsInvalidArguments(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
	}{
		{"signin", []string{"unexpected"}},
		{"status", []string{"unexpected"}},
		{"select", nil},
		{"select", []string{"first", "second"}},
		{"signout", []string{"first", "second"}},
		{"models", []string{"unexpected"}},
		{"prepare-host", []string{"unexpected"}},
	} {
		t.Run(tc.command+"/"+strings.Join(tc.args, ","), func(t *testing.T) {
			var out bytes.Buffer
			cmd := newChatGPTCommand(&out)
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(append([]string{tc.command}, tc.args...))
			err := cmd.Execute()
			if err == nil {
				t.Fatal("accepted invalid arguments")
			}
			if !strings.Contains(err.Error(), "unknown command") && !strings.Contains(err.Error(), "accepts") {
				t.Fatalf("unexpected argument error: %v", err)
			}
		})
	}
}

func TestChatGPTEmptyStatus(t *testing.T) {
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
