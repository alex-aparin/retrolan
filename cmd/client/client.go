package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"retrolan/internal/utils"
	"sync/atomic"
	"syscall"
)

// nolint:cyclop
func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	inbound := make(chan []byte, 100)
	outbound := make(chan []byte, 100)

	go runPeerConnection(ctx, cancel, inbound, outbound)

	gameAddr := utils.GetEnv("GAME_SERVER_ADDR", ":27015")
	udpAddr, err := net.ResolveUDPAddr("udp", gameAddr)
	if err != nil {
		slog.Error("resolve UDP addr", "addr", gameAddr, "err", err)
		os.Exit(1)
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		slog.Error("listen UDP", "err", err)
		os.Exit(1)
	}
	slog.Info("UDP listener bound", "addr", conn.LocalAddr().String())

	// Closing the conn from a watcher goroutine unblocks ReadFromUDP below
	// when ctx is cancelled (signal received or peer connection failed).
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	// The remote peer has no way to learn the game's UDP address, so the client
	// remembers the source of the most recent inbound datagram and replies there.
	var lastAddr atomic.Pointer[net.UDPAddr]

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-inbound:
				dst := lastAddr.Load()
				if dst == nil {
					continue
				}
				if _, err := conn.WriteToUDP(msg, dst); err != nil {
					slog.Warn("UDP write failed", "dst", dst.String(), "err", err)
				}
			}
		}
	}()

	for {
		buf := make([]byte, 2048)
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				slog.Info("shutting down UDP listener")
				return
			}
			slog.Error("UDP read failed", "err", err)
			return
		}
		lastAddr.Store(addr)
		select {
		case outbound <- buf[:n]:
		case <-ctx.Done():
			return
		}
	}
}
