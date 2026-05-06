package main

import (
	"context"
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
func runPeerConnection(ctx context.Context, cancel context.CancelFunc, inputMessages, outputMessages chan []byte) {
	// Everything below is the Pion WebRTC API! Thanks for using it ❤️.
	address := &net.UDPAddr{
		IP: net.IP{127, 0, 0, 2},
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

	dataChannel, err := peerConnection.CreateDataChannel("data", nil)
	if err != nil {
		panic(err)
	}

	dataChannel.OnOpen(func() {
		slog.Info("data channel open", "label", dataChannel.Label(), "id", dataChannel.ID())
		for {
			select {
			case <-ctx.Done():
				return
			case buf := <-outputMessages:
				if sendErr := dataChannel.Send(buf); sendErr != nil {
					slog.Error("data channel send failed", "err", sendErr)
					return
				}
			}
		}
	})

	dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
		select {
		case inputMessages <- msg.Data:
		case <-ctx.Done():
		}
	})

	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	}

	// Subscribe to ICE gathering completion BEFORE SetLocalDescription starts it,
	// so we can serialize the full SDP (with candidates) — there is no trickle
	// channel between the two processes.
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	err = peerConnection.SetLocalDescription(offer)
	if err != nil {
		panic(err)
	}

	select {
	case <-gatherComplete:
	case <-ctx.Done():
		return
	}

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
	for len(answerFiles) == 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second * 2):
		}
		answerFiles, err = utils.ScanFilesWithPattern(dir, "answer*")
	}
	decode(string(answerFiles[0].Content), &answer)

	err = peerConnection.SetRemoteDescription(answer)
	if err != nil {
		panic(err)
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
