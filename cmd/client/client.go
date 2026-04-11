package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pion/randutil"
	"github.com/pion/webrtc/v4"
)

// FileContent represents the content of a scanned file
type FileContent struct {
	Path    string
	Name    string
	Content []byte
	Error   error
}

// ScanFilesWithPattern scans for files matching a glob pattern
func ScanFilesWithPattern(rootDir string, pattern string) ([]FileContent, error) {
	var results []FileContent

	// Use filepath.Glob to find matching files
	matches, err := filepath.Glob(filepath.Join(rootDir, pattern))
	if err != nil {
		return nil, err
	}

	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			results = append(results, FileContent{
				Path:  path,
				Error: err,
			})
			continue
		}

		if info.IsDir() {
			continue
		}

		content, err := os.ReadFile(path)
		results = append(results, FileContent{
			Path:    path,
			Name:    info.Name(),
			Content: content,
			Error:   err,
		})
	}

	failedFilesCount := 0
	for i, v := range results {
		if err := os.Remove(v.Path); err != nil {
			failedFilesCount++
			// TODO: (alex) add logging
		} else {
			results[i-failedFilesCount] = v
		}
	}

	return results[:len(results)-failedFilesCount], nil
}

func GetEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

var IsServer bool

// nolint:cyclop
func main() {
	IsServer = GetEnv("TYPE", "SERVER") == "SERVER"
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
	peerConnection, err := webrtc.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnection: %v\n", cErr)
		}
	}()

	// Set the handler for Peer connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		fmt.Printf("Peer Connection State has changed: %s\n", state.String())

		if state == webrtc.PeerConnectionStateFailed {
			// Wait until PeerConnection has had no network activity for 30 seconds or another failure.
			// It may be reconnected using an ICE Restart.
			// Use webrtc.PeerConnectionStateDisconnected if you are interested in detecting faster timeout.
			// Note that the PeerConnection may come back from PeerConnectionStateDisconnected.
			fmt.Println("Peer Connection has gone to failed exiting")
			os.Exit(0)
		}

		if state == webrtc.PeerConnectionStateClosed {
			// PeerConnection was explicitly closed. This usually happens from a DTLS CloseNotify
			fmt.Println("Peer Connection has gone to closed exiting")
			os.Exit(0)
		}
	})

	// Register data channel creation handling
	peerConnection.OnDataChannel(func(dataChannel *webrtc.DataChannel) {
		fmt.Printf("New DataChannel %s %d\n", dataChannel.Label(), dataChannel.ID())

		// Register channel opening handling
		dataChannel.OnOpen(func() {
			fmt.Printf(
				"Data channel '%s'-'%d' open. Random messages will now be sent to any connected DataChannels every 5 seconds\n",
				dataChannel.Label(), dataChannel.ID(),
			)

			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				message, sendErr := randutil.GenerateCryptoRandomString(15, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
				if sendErr != nil {
					panic(sendErr)
				}

				// Send the message as text
				fmt.Printf("Sending '%s'\n", message)
				if sendErr = dataChannel.SendText(message); sendErr != nil {
					panic(sendErr)
				}
			}
		})

		// Register text message handling
		dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
			fmt.Printf("Message from DataChannel '%s': '%s'\n", dataChannel.Label(), string(msg.Data))
		})
	})

	if IsServer {

		// Wait for the offer to be pasted
		offer := webrtc.SessionDescription{}
		var offerFiles []FileContent
		for offerFiles == nil || len(offerFiles) == 0 {
			time.Sleep(time.Second * 2)
			dir, err := os.Getwd()
			if err != nil {
				continue
			}
			offerFiles, err = ScanFilesWithPattern(dir, "offer*")
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

		// Create channel that is blocked until ICE Gathering is complete
		gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

		// Sets the LocalDescription, and starts our UDP listeners
		err = peerConnection.SetLocalDescription(answer)
		if err != nil {
			panic(err)
		}

		// Block until ICE Gathering is complete, disabling trickle ICE
		// we do this because we only can exchange one signaling message
		// in a production application you should exchange ICE Candidates via OnICECandidate
		<-gatherComplete

		// Output the answer in base64 so we can paste it in browser
		fmt.Println(encode(peerConnection.LocalDescription()))
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
