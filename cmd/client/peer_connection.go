package main

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"retrolan/internal/utils"
	"time"

	"github.com/pion/webrtc/v4"
)

// nolint:cyclop
func runPeerConnection(inputMessages chan []byte, outputMessages chan []byte) {
	// Everything below is the Pion WebRTC API! Thanks for using it ❤️.
	address := &net.UDPAddr{
		IP: net.IP{127, 0, 0, 2},
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

	// Register data channel creation handling
	peerConnection.OnDataChannel(func(dataChannel *webrtc.DataChannel) {
		slog.Info("new data channel", "label", dataChannel.Label(), "id", dataChannel.ID())

		// Register channel opening handling
		dataChannel.OnOpen(func() {
			slog.Info("data channel open", "label", dataChannel.Label(), "id", dataChannel.ID())
			for buf := range outputMessages {
				if sendErr := dataChannel.Send(buf); sendErr != nil {
					panic(sendErr)
				}
			}
		})

		// Register text message handling
		dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
			inputMessages <- msg.Data
		})
	})
	_, err = peerConnection.CreateDataChannel("data", nil)
	if err != nil {
		panic(err)
	}

	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	}

	// Subscribe to ICE gathering completion BEFORE SetLocalDescription starts it,
	// so we can serialize the full SDP (with candidates) — there is no trickle
	// channel between the two processes.
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(offer)
	if err != nil {
		panic(err)
	}

	<-gatherComplete

	dir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	err = utils.SaveStringToFile(encode(peerConnection.LocalDescription()), filepath.Join(dir, "offer1"))
	if err != nil {
		slog.Warn("failed to write offer file", "path", "offer1", "err", err)
	}

	answer := webrtc.SessionDescription{}
	var answerFiles []utils.FileContent
	for answerFiles == nil || len(answerFiles) == 0 {
		time.Sleep(time.Second * 2)
		answerFiles, err = utils.ScanFilesWithPattern(dir, "answer*")
	}
	decode(string(answerFiles[0].Content), &answer)

	err = peerConnection.SetRemoteDescription(answer)
	if err != nil {
		panic(err)
	}

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
