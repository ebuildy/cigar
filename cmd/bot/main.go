// Command bot is the gitlab-ci-resources-bot CLI. `bot serve` runs the HTTP
// service: GitLab Pipeline webhooks on :8080, health on :8081.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"go.uber.org/zap"
	"golang.org/x/term"

	"github.com/spf13/cobra"
)

// version is stamped by goreleaser via -ldflags "-X main.version=...".
var version = "dev"

// logger is the process-wide zap logger built from the --log-level flag in the
// root command's PersistentPreRunE, before any subcommand runs.
var logger *zap.Logger

var coloredBanner = []string{
	"\x1b[38;5;208m   /\\             /\\\x1b[38;5;214m                      *\x1b[0m",
	"\x1b[38;5;208m  /  \\___________/  \\\x1b[38;5;214m                  .:'\x1b[0m",
	"\x1b[38;5;208m |    \\         /    |\x1b[38;5;214m               .:'\x1b[0m",
	"\x1b[38;5;208m |  __,          __,  |\x1b[38;5;214m            .:'\x1b[0m",
	"\x1b[38;5;208m |  \\_`          \\_`  |\x1b[38;5;214m          ,:'\x1b[0m",
	"\x1b[38;5;208m  \\       . .        /\x1b[38;5;214m         ,'\x1b[0m",
	"\x1b[38;5;208m   \\      `-'       /\x1b[38;5;214m \x1b[38;5;245m______\x1b[38;5;214m ,'\x1b[0m",
	"\x1b[38;5;208m    `.            ,'\x1b[38;5;245m-[_|_|__\x1b[38;5;196m(:>\x1b[0m",
	"\x1b[38;5;208m      `.________,'\x1b[38;5;214m\x1b[0m",
	"\x1b[1m\x1b[38;5;208m          _\x1b[0m",
	"\x1b[1m\x1b[38;5;208m     ____(_)__ ____ _____\x1b[0m",
	"\x1b[1m\x1b[38;5;208m    / __/ / _ `/ _ `/ __/\x1b[0m",
	"\x1b[1m\x1b[38;5;208m    \\__/_/\\_, /\\_,_/_/\x1b[0m",
	"\x1b[1m\x1b[38;5;208m         /___/\x1b[0m",
	"\x1b[38;5;240m   your pipelines, smoked & measured\x1b[0m",
}

var plainBanner = []string{
	"   /\\             /\\                      *",
	"  /  \\___________/  \\                  .:'",
	" |    \\         /    |               .:'",
	" |  __,          __,  |            .:'",
	" |  \\_`          \\_`  |          ,:'",
	"  \\       . .        /         ,'",
	"   \\      `-'       / ______ ,'",
	"    `.            ,'-[_|_|__(:>",
	"      `.________,'",
	"          _",
	"     ____(_)__ ____ _____",
	"    / __/ / _ `/ _ `/ __/",
	"    \\__/_/\\_, /\\_,_/_/",
	"         /___/",
	"   your pipelines, smoked & measured",
}

func main() {
	var logLevel string
	root := &cobra.Command{
		Use:           "bot",
		Short:         "Posts CI pipeline resource-usage reports as GitLab MR comments",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if term.IsTerminal(int(os.Stdout.Fd())) {
				_, _ = os.Stdout.WriteString(banner(true))
			}
			log, err := newLogger(logLevel)
			if err != nil {
				return err
			}
			logger = log
			return nil
		},
	}
	root.PersistentFlags().StringVar(&logLevel, "log-level", envOr("LOG_LEVEL", "info"),
		"log verbosity: debug, info, warn or error (defaults to $LOG_LEVEL, then info)")
	root.AddCommand(newServeCmd(), newRunCmd())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := root.ExecuteContext(ctx); err != nil {
		if logger != nil {
			logger.Error("fatal", zap.Error(err))
			_ = logger.Sync()
		} else {
			fmt.Fprintln(os.Stderr, "fatal:", err)
		}
		os.Exit(1)
	}
	if logger != nil {
		_ = logger.Sync()
	}
}

// envOr returns the value of the named environment variable, or def when unset
// or empty. Used to default the --log-level flag from $LOG_LEVEL.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// banner returns the banner. Colors are dropped when color is false
// or the NO_COLOR convention (https://no-color.org) is set.
func banner(color bool) string {
	if _, noColor := os.LookupEnv("NO_COLOR"); noColor {
		color = false
	}
	lines := plainBanner
	if color {
		lines = coloredBanner
	}
	return strings.Join(lines, "\n") + "\n"
}
