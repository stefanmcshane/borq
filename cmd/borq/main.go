package main

import (
	"context"
	"os"

	"github.com/stefanmcshane/borq/api/cli"
	"github.com/stefanmcshane/borq/foundations/logger"
)

func main() {
	ctx := context.Background()

	log, err := logger.NewCliLogger()
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
