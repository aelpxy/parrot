package main

import (
	"context"
	"os"
	"os/signal"

	"golang.org/x/sys/unix"

	"github.com/aelpxy/parrot/internal/cli"
)

func main() {
	app := cli.New(os.Stdin, os.Stdout, os.Stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	code := app.Run(ctx, os.Args[1:])

	stop()
	os.Exit(code)
}
