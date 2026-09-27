package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap/zapcore"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected zapcore.Level
	}{
		{name: "debug", input: "debug", expected: zapcore.DebugLevel},
		{name: "info", input: "info", expected: zapcore.InfoLevel},
		{name: "warn", input: "warn", expected: zapcore.WarnLevel},
		{name: "warning", input: "warning", expected: zapcore.WarnLevel},
		{name: "error", input: "error", expected: zapcore.ErrorLevel},
		{name: "empty defaults info", input: "", expected: zapcore.InfoLevel},
		{name: "unknown defaults info", input: "unknown", expected: zapcore.InfoLevel},
		{name: "mixed case debug", input: "DEBUG", expected: zapcore.DebugLevel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := parseLogLevel(tt.input)
			if got != tt.expected {
				t.Fatalf("parseLogLevel(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestResolveLlamaServerBin(t *testing.T) {
	// Save and restore the original statFunc and defaultLlamaServerPaths
	origStat := statFunc
	origPaths := defaultLlamaServerPaths
	t.Cleanup(func() {
		statFunc = origStat
		defaultLlamaServerPaths = origPaths
	})

	t.Run("explicit override is returned as-is", func(t *testing.T) {
		got, err := resolveLlamaServerBin("/custom/path/llama-server")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/custom/path/llama-server" {
			t.Fatalf("got %q, want /custom/path/llama-server", got)
		}
	})

	t.Run("finds first candidate", func(t *testing.T) {
		defaultLlamaServerPaths = []string{"/first/llama-server", "/second/llama-server"}
		statFunc = func(name string) (os.FileInfo, error) {
			if name == "/first/llama-server" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}

		got, err := resolveLlamaServerBin("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/first/llama-server" {
			t.Fatalf("got %q, want /first/llama-server", got)
		}
	})

	t.Run("falls through to second candidate", func(t *testing.T) {
		defaultLlamaServerPaths = []string{"/first/llama-server", "/second/llama-server"}
		statFunc = func(name string) (os.FileInfo, error) {
			if name == "/second/llama-server" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}

		got, err := resolveLlamaServerBin("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/second/llama-server" {
			t.Fatalf("got %q, want /second/llama-server", got)
		}
	})

	t.Run("returns error when no candidate found", func(t *testing.T) {
		defaultLlamaServerPaths = []string{"/nope/llama-server"}
		statFunc = func(string) (os.FileInfo, error) {
			return nil, errors.New("not found")
		}

		_, err := resolveLlamaServerBin("")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestResolveTensorFoldBin(t *testing.T) {
	origStat := statFunc
	origPaths := defaultTensorFoldPaths
	origHome := userHomeDir
	t.Cleanup(func() {
		statFunc = origStat
		defaultTensorFoldPaths = origPaths
		userHomeDir = origHome
	})
	userHomeDir = func() (string, error) { return "/Users/tester", nil }

	t.Run("explicit override is returned as-is", func(t *testing.T) {
		statFunc = func(string) (os.FileInfo, error) { return nil, errors.New("not found") }
		got, err := resolveTensorFoldBin("/custom/tensorfold")
		if err != nil || got != "/custom/tensorfold" {
			t.Fatalf("got %q, %v; want /custom/tensorfold", got, err)
		}
	})

	t.Run("expands ~ and finds the uv tool install first", func(t *testing.T) {
		defaultTensorFoldPaths = []string{"~/.local/bin/tensorfold", "/opt/homebrew/bin/tensorfold"}
		var probed []string
		statFunc = func(name string) (os.FileInfo, error) {
			probed = append(probed, name)
			if name == "/Users/tester/.local/bin/tensorfold" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}
		got, err := resolveTensorFoldBin("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/Users/tester/.local/bin/tensorfold" {
			t.Fatalf("got %q (probed %v), want /Users/tester/.local/bin/tensorfold", got, probed)
		}
	})

	t.Run("falls through to homebrew", func(t *testing.T) {
		defaultTensorFoldPaths = []string{"~/.local/bin/tensorfold", "/opt/homebrew/bin/tensorfold"}
		statFunc = func(name string) (os.FileInfo, error) {
			if name == "/opt/homebrew/bin/tensorfold" {
				return nil, nil
			}
			return nil, errors.New("not found")
		}
		got, err := resolveTensorFoldBin("")
		if err != nil || got != "/opt/homebrew/bin/tensorfold" {
			t.Fatalf("got %q, %v; want /opt/homebrew/bin/tensorfold", got, err)
		}
	})

	t.Run("error names the install command and the flag", func(t *testing.T) {
		defaultTensorFoldPaths = []string{"~/.local/bin/tensorfold"}
		statFunc = func(string) (os.FileInfo, error) { return nil, errors.New("not found") }
		_, err := resolveTensorFoldBin("")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		for _, want := range []string{"uv tool install", "--tensorfold-bin"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q: %v", want, err)
			}
		}
	})
}
