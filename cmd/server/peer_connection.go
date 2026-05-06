package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"retrolan/internal/utils"
	"time"

	"github.com/pion/webrtc/v4"
)

// nolint:cyclop
func runPeerConnection(ctx context.Context, cancel context.CancelFunc) {
	address := &net.UDPAddr{
		IP: net.IP{127, 0, 0, 3},
	}

	udpListener, err := net.ListenUDP("udp", address)
	if err != nil {
		panic(err)
	}

	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetICEUDPMux(webrtc.NewICEUDPMux(nil, udpListener))
	// Pion skips loopback interfaces when gathering host candidates by default.
	// Required when the UDP mux is bound to a 127.0.0.0/8 address; harmless
	// otherwise, since non-loopback interfaces are still gathered.
	settingEngine.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine))

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			slog.Error("close peer connection failed", "err", cErr)
		}
	}()

	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		slog.Info("peer connection state changed", "state", state.String())

		switch state {
		case webrtc.PeerConnectionStateFailed:
			slog.Error("peer connection failed")
			cancel()
		case webrtc.PeerConnectionStateClosed:
			slog.Info("peer connection closed")
			cancel()
		}
	})

	peerConnection.OnDataChannel(func(dataChannel *webrtc.DataChannel) {
		slog.Info("new data channel", "label", dataChannel.Label(), "id", dataChannel.ID())

		// Dial a fresh UDP socket to the game server for this data channel so
		// the game server sees a unique source port per relayed peer; lifetime
		// is bound to the data channel via dcCtx below.
		gameAddr := utils.GetEnv("GAME_SERVER_ADDR", "127.0.0.1:27015")
		gameConn, err := net.Dial("udp", gameAddr)
		if err != nil {
			slog.Error("dial game server failed", "addr", gameAddr, "err", err)
			return
		}
		slog.Info("game server conn dialed",
			"label", dataChannel.Label(), "id", dataChannel.ID(),
			"local", gameConn.LocalAddr().String(), "remote", gameConn.RemoteAddr().String())

		dcCtx, dcCancel := context.WithCancel(ctx)

		// Closes gameConn when the parent context cancels or the data channel
		// closes, unblocking the read loop in OnOpen.
		go func() {
			<-dcCtx.Done()
			_ = gameConn.Close()
		}()

		dataChannel.OnClose(func() {
			slog.Info("data channel closed", "label", dataChannel.Label(), "id", dataChannel.ID())
			dcCancel()
		})

		dataChannel.OnOpen(func() {
			slog.Info("data channel open, relaying to game server",
				"label", dataChannel.Label(), "id", dataChannel.ID(), "game_addr", gameAddr)

			go func() {
				buf := make([]byte, 2048)
				for {
					n, err := gameConn.Read(buf)
					if err != nil {
						if dcCtx.Err() != nil || errors.Is(err, net.ErrClosed) {
							return
						}
						slog.Error("game server read failed", "err", err)
						return
					}
					if sendErr := dataChannel.Send(buf[:n]); sendErr != nil {
						slog.Error("data channel send failed", "err", sendErr)
						return
					}
				}
			}()
		})

		dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
			if _, err := gameConn.Write(msg.Data); err != nil {
				slog.Warn("game server write failed", "err", err)
			}
		})
	})

	dir, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	offer := webrtc.SessionDescription{}
	var offerFiles []utils.FileContent
	for len(offerFiles) == 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second * 2):
		}
		offerFiles, err = utils.ScanFilesWithPattern(dir, "offer*")
	}
	decode(string(offerFiles[0].Content), &offer)

	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		panic(err)
	}

	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Subscribe to ICE gathering completion BEFORE SetLocalDescription starts it.
	// We block until gathering is complete, disabling trickle ICE: the file-based
	// handoff can only carry one signaling message, so the answer must already
	// contain every candidate by the time it is written.
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	select {
	case <-gatherComplete:
	case <-ctx.Done():
		return
	}

	err = utils.SaveStringToFile(encode(peerConnection.LocalDescription()), filepath.Join(dir, "answer1"))
	if err != nil {
		slog.Warn("failed to write answer file", "path", "answer1", "err", err)
	}

	<-ctx.Done()
}

// JSON encode + base64 a SessionDescription.
func encode(obj *webrtc.SessionDescription) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(b)
}

// Decode a base64 and unmarshal JSON into a SessionDescription.
func decode(in string, obj *webrtc.SessionDescription) {
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		panic(err)
	}

	if err = json.Unmarshal(b, obj); err != nil {
		panic(err)
	}
}
