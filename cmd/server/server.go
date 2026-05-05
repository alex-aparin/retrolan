package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"retrolan/internal/utils"
	"syscall"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	gameAddr := utils.GetEnv("GAME_SERVER_ADDR", "127.0.0.1:27015")
	gameConn, err := net.Dial("udp", gameAddr)
	if err != nil {
		panic(fmt.Errorf("dial game server %s: %w", gameAddr, err))
	}
	defer func() {
		if cErr := gameConn.Close(); cErr != nil {
			slog.Error("close game server conn failed", "err", cErr)
		}
	}()
	slog.Info("game server conn dialed", "addr", gameConn.RemoteAddr().String())

	inbound := make(chan []byte, 100)
	outbound := make(chan []byte, 100)

	go runPeerConnection(ctx, cancel, inbound, outbound)

	// Closing the conn from a watcher goroutine unblocks gameConn.Read below
	// when ctx is cancelled (signal received or peer connection failed).
	go func() {
		<-ctx.Done()
		_ = gameConn.Close()
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-inbound:
				if _, err := gameConn.Write(msg); err != nil {
					slog.Warn("game server write failed", "err", err)
				}
			}
		}
	}()

	for {
		buf := make([]byte, 2048)
		n, err := gameConn.Read(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				slog.Info("shutting down game server reader")
				return
			}
			slog.Error("game server read failed", "err", err)
			return
		}
		select {
		case outbound <- buf[:n]:
		case <-ctx.Done():
			return
		}
	}
}
