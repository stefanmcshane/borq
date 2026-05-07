package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/stefanmcshane/borq/api/cli"
	"github.com/stefanmcshane/borq/foundations/logger"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log, err := logger.NewLogger()
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	defer func() {
		_ = log.Sync()
	}()

	command := cli.NewRootCommand(ctx, log)
	if err := command.Execute(); err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
}
