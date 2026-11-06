package main

import (
	"net"
	"path/filepath"
	"testing"
)

func TestRunExitCodes(t *testing.T) {
	missingFile := filepath.Join(t.TempDir(), "chatgpt.json")

	cases := []struct {
		name      string
		arguments []string
		want      int
	}{
		{"help", []string{"-help"}, 0},
		{"help for a command", []string{"login", "-help"}, 0},
		{"unknown flag", []string{"-unknown"}, 2},
		{"invalid value", []string{"-listen", "invalid"}, 2},
		{"command after options", []string{"-account", "work", "login"}, 2},
		// These options keep a broken check away from a real port and credential file.
		{
			"reconsent without login",
			[]string{"-reconsent", "-listen", "127.0.0.1:0", "-auth-file", missingFile}, 2,
		},
		{
			"reconsent with another command",
			[]string{"logout", "-reconsent", "-auth-file", missingFile}, 2,
		},
		{"failed authentication command", []string{"login-status", "-auth-file", missingFile}, 1},
		{
			"ChatGPT mode without credentials",
			[]string{"-auth-mode", "chatgpt", "-auth-file", missingFile}, 1,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clearConfigEnvironment(t)

			if got := run(testCase.arguments); got != testCase.want {
				t.Errorf("exit code = %d, want %d", got, testCase.want)
			}
		})
	}
}

func TestOpenListeners(t *testing.T) {
	t.Run("opens every address", func(t *testing.T) {
		listeners, err := openListeners([]string{"127.0.0.1:0", "127.0.0.1:0"})
		if err != nil {
			t.Fatalf("openListeners: %v", err)
		}
		defer func() {
			for _, listener := range listeners {
				listener.Close()
			}
		}()

		if len(listeners) != 2 {
			t.Errorf("got %d listeners, want 2", len(listeners))
		}
	})

	t.Run("reports an occupied address", func(t *testing.T) {
		occupied, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer occupied.Close()

		listeners, err := openListeners([]string{"127.0.0.1:0", occupied.Addr().String()})
		if err == nil {
			t.Fatal("error = nil")
		}
		if listeners != nil {
			t.Errorf("listeners = %v, want none after a failure", listeners)
		}
	})
}
