// Command finfocus-plugin-scaleway-public serves Scaleway projected costs to
// FinFocus over gRPC.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/rshade/finfocus-spec/sdk/go/pluginsdk"

	"github.com/mkutlak/finfocus-plugin-scaleway-public/internal/pricing"
)

func main() {
	flag.Parse()

	plugin := pricing.NewCalculator()
	logger := pluginsdk.NewPluginLogger(
		plugin.Name(), pricing.PluginVersion, parseLogLevel(pluginsdk.GetLogLevel()), pluginsdk.NewLogWriter(),
	)

	port := pluginsdk.ParsePortFlag()
	if port == 0 {
		port = pluginsdk.GetPort()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info().Int("port", port).Msg("Starting plugin")
	if err := pluginsdk.Serve(ctx, pluginsdk.ServeConfig{Plugin: plugin, Port: port}); err != nil {
		logger.Error().Err(err).Msg("Failed to serve plugin")
		stop()
		os.Exit(1)
	}
}

func parseLogLevel(levelStr string) zerolog.Level {
	switch strings.ToLower(levelStr) {
	case "trace":
		return zerolog.TraceLevel
	case "debug":
		return zerolog.DebugLevel
	case "warn", "warning":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	default:
		return zerolog.InfoLevel
	}
}
