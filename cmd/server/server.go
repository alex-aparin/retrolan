package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	runPeerConnection(ctx, cancel)
}
