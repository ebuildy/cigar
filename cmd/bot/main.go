// Command bot is the gitlab-ci-resources-bot CLI. `bot serve` runs the HTTP
// service: GitLab Pipeline webhooks on :8080, health on :8081.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// version is stamped by goreleaser via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	root := &cobra.Command{
		Use:           "bot",
		Short:         "Posts CI pipeline resource-usage reports as GitLab MR comments",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newServeCmd(), newRunCmd())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := root.ExecuteContext(ctx); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}
