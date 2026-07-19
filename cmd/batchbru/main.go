package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ikaro1192/batchbru"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := batchbru.Run(ctx, os.Args[1:]); err != nil {
		if !batchbru.IsExitCode(err) {
			fmt.Fprintln(os.Stderr, "batchbru: "+err.Error())
		}
		os.Exit(1)
	}
}
