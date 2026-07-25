package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// newTestRoot returns a root command with config flags bound and the given args.
func newTestRoot(args ...string) *cobra.Command {
	root := &cobra.Command{Use: "bot", RunE: func(*cobra.Command, []string) error { return nil }}
	BindFlags(root)
	root.SetArgs(args)
	return root
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestResolvePrecedence(t *testing.T) {
	// Value quoted so YAML keeps it a string: an unquoted 0.10 is parsed as a
	// float and viper.GetString renders it back as "0.1", which would make the
	// assertion brittle without changing what precedence is being verified.
	file := writeConfig(t, "report:\n  throttle_warn_ratio: \"0.10\"\n")

	t.Run("file value used when no env or flag", func(t *testing.T) {
		root := newTestRoot("--config", file)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		v, err := Resolve(root)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got := v.GetString("report.throttle_warn_ratio"); got != "0.10" {
			t.Fatalf("got %q, want file value 0.10", got)
		}
	})

	t.Run("env overrides file", func(t *testing.T) {
		t.Setenv("REPORT_THROTTLE_WARN_RATIO", "0.20")
		root := newTestRoot("--config", file)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		v, err := Resolve(root)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got := v.GetString("report.throttle_warn_ratio"); got != "0.20" {
			t.Fatalf("got %q, want env value 0.20", got)
		}
	})

	t.Run("flag overrides env and file", func(t *testing.T) {
		t.Setenv("REPORT_THROTTLE_WARN_RATIO", "0.20")
		root := newTestRoot("--config", file, "--report-throttle-warn-ratio", "0.30")
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		v, err := Resolve(root)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got := v.GetString("report.throttle_warn_ratio"); got != "0.30" {
			t.Fatalf("got %q, want flag value 0.30", got)
		}
	})
}

func TestResolveMissingExplicitFileErrors(t *testing.T) {
	root := newTestRoot("--config", "/no/such/config.yaml")
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(root); err == nil {
		t.Fatal("Resolve succeeded with a missing explicit --config path")
	}
}

func TestResolveNoFileFallsBackToEnv(t *testing.T) {
	// No --config, and t.TempDir cwd has no config.yaml -> env/defaults only.
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("REPORT_THROTTLE_WARN_RATIO", "0.42")
	root := newTestRoot()
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	v, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := v.GetString("report.throttle_warn_ratio"); got != "0.42" {
		t.Fatalf("got %q, want env value 0.42", got)
	}
}
