// Command bot is the gitlab-ci-resources-bot CLI. `bot serve` runs the HTTP
// service: GitLab Pipeline webhooks on :8080, health on :8081.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/spf13/cobra"
)

// version is stamped by goreleaser via -ldflags "-X main.version=...".
var version = "dev"

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
	root := &cobra.Command{
		Use:           "bot",
		Short:         "Posts CI pipeline resource-usage reports as GitLab MR comments",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if term.IsTerminal(int(os.Stdout.Fd())) {
				os.Stdout.WriteString(banner(true))
			}
		},
	}
	root.AddCommand(newServeCmd(), newRunCmd())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := root.ExecuteContext(ctx); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
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
