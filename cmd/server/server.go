package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"retrolan/internal/utils"
	"time"

	"github.com/pion/webrtc/v4"
)

// nolint:cyclop
func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	address := &net.UDPAddr{
		IP: net.IP{127, 0, 0, 3},
	}

	// 2. Create the UDP listener.
	udpListener, err := net.ListenUDP("udp", address)
	if err != nil {
		panic(err)
	}

	// 3. Configure the SettingEngine with the UDP Mux.
	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetICEUDPMux(webrtc.NewICEUDPMux(nil, udpListener))
	// Pion skips loopback interfaces when gathering host candidates by default.
	// Required when the UDP mux is bound to a 127.0.0.0/8 address; harmless
	// otherwise, since non-loopback interfaces are still gathered.
	settingEngine.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine))
	// Everything below is the Pion WebRTC API! Thanks for using it ❤️.

	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			slog.Error("close peer connection failed", "err", cErr)
		}
	}()

	// Set the handler for Peer connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		slog.Info("peer connection state changed", "state", state.String())

		if state == webrtc.PeerConnectionStateFailed {
			// Wait until PeerConnection has had no network activity for 30 seconds or another failure.
			// It may be reconnected using an ICE Restart.
			// Use webrtc.PeerConnectionStateDisconnected if you are interested in detecting faster timeout.
			// Note that the PeerConnection may come back from PeerConnectionStateDisconnected.
			slog.Error("peer connection failed, exiting")
			os.Exit(0)
		}

		if state == webrtc.PeerConnectionStateClosed {
			// PeerConnection was explicitly closed. This usually happens from a DTLS CloseNotify
			slog.Info("peer connection closed, exiting")
			os.Exit(0)
		}
	})

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

	peerConnection.OnDataChannel(func(dataChannel *webrtc.DataChannel) {
		slog.Info("new data channel", "label", dataChannel.Label(), "id", dataChannel.ID())

		dataChannel.OnOpen(func() {
			slog.Info("data channel open, relaying to game server",
				"label", dataChannel.Label(), "id", dataChannel.ID(), "game_addr", gameAddr)

			go func() {
				buf := make([]byte, 2048)
				for {
					n, err := gameConn.Read(buf)
					if err != nil {
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

	// Wait for the offer to appear on disk
	offer := webrtc.SessionDescription{}
	var offerFiles []utils.FileContent
	for offerFiles == nil || len(offerFiles) == 0 {
		time.Sleep(time.Second * 2)
		offerFiles, err = utils.ScanFilesWithPattern(dir, "offer*")
	}
	decode(string(offerFiles[0].Content), &offer)

	// Set the remote SessionDescription
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		panic(err)
	}

	// Create an answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Subscribe to ICE gathering completion BEFORE SetLocalDescription starts it.
	// We block until gathering is complete, disabling trickle ICE: the file-based
	// handoff can only carry one signaling message, so the answer must already
	// contain every candidate by the time it is written.
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	<-gatherComplete

	err = utils.SaveStringToFile(encode(peerConnection.LocalDescription()), filepath.Join(dir, "answer1"))
	if err != nil {
		slog.Warn("failed to write answer file", "path", "answer1", "err", err)
	}

	// Block forever
	select {}
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
