package main

import (
	"log/slog"
	"net"
	"os"
	"sync/atomic"
)

// nolint:cyclop
func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	inbound := make(chan []byte, 100)
	outbound := make(chan []byte, 100)

	go runPeerConnection(inbound, outbound)

	udpAddr, err := net.ResolveUDPAddr("udp", ":27015")
	if err != nil {
		slog.Error("resolve UDP addr", "err", err)
		os.Exit(1)
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		slog.Error("listen UDP", "err", err)
		os.Exit(1)
	}
	slog.Info("UDP listener bound", "addr", conn.LocalAddr().String())

	// The remote peer has no way to learn the game's UDP address, so the client
	// remembers the source of the most recent inbound datagram and replies there.
	var lastAddr atomic.Pointer[net.UDPAddr]

	go func() {
		for msg := range inbound {
			dst := lastAddr.Load()
			if dst == nil {
				continue
			}
			if _, err := conn.WriteToUDP(msg, dst); err != nil {
				slog.Warn("UDP write failed", "dst", dst.String(), "err", err)
			}
		}
	}()

	for {
		buf := make([]byte, 2048)
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			slog.Error("UDP read failed", "err", err)
			return
		}
		lastAddr.Store(addr)
		outbound <- buf[:n]
	}
}
