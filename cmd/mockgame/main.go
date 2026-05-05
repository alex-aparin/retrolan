package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"retrolan/internal/utils"
	"strconv"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML string

type event struct {
	Timestamp time.Time `json:"timestamp"`
	Socket    string    `json:"socket"`    // "listen" or "send"
	Direction string    `json:"direction"` // "rx" or "tx"
	Peer      string    `json:"peer"`
	Length    int       `json:"length"`
	Preview   string    `json:"preview"`
}

func newEvent(socket, direction, peer string, payload []byte) event {
	return event{
		Timestamp: time.Now(),
		Socket:    socket,
		Direction: direction,
		Peer:      peer,
		Length:    len(payload),
		Preview:   strconv.Quote(string(payload)),
	}
}

// hub fans out events to every connected SSE subscriber.
type hub struct {
	mu   sync.Mutex
	subs map[chan event]struct{}
}

func newHub() *hub {
	return &hub{subs: make(map[chan event]struct{})}
}

func (h *hub) subscribe() chan event {
	ch := make(chan event, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *hub) broadcast(ev event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		// Drop on slow subscribers rather than blocking the UDP read loops.
		select {
		case ch <- ev:
		default:
		}
	}
}

func runListener(ctx context.Context, addr string, h *hub) error {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("resolve listen addr %q: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listen UDP %q: %w", addr, err)
	}
	slog.Info("listener bound", "addr", conn.LocalAddr().String())

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	buf := make([]byte, 65536)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("listener read: %w", err)
		}
		payload := append([]byte(nil), buf[:n]...)
		h.broadcast(newEvent("listen", "rx", src.String(), payload))

		if _, err := conn.WriteToUDP(payload, src); err != nil {
			slog.Warn("listener echo failed", "dst", src.String(), "err", err)
			continue
		}
		h.broadcast(newEvent("listen", "tx", src.String(), payload))
	}
}

type sender struct {
	conn   *net.UDPConn
	target *net.UDPAddr
}

func newSender(targetAddr string) (*sender, error) {
	target, err := net.ResolveUDPAddr("udp", targetAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve target %q: %w", targetAddr, err)
	}
	conn, err := net.ListenUDP("udp", nil) // ephemeral local port
	if err != nil {
		return nil, fmt.Errorf("open sender socket: %w", err)
	}
	return &sender{conn: conn, target: target}, nil
}

func (s *sender) localAddr() string  { return s.conn.LocalAddr().String() }
func (s *sender) targetAddr() string { return s.target.String() }

func (s *sender) send(payload []byte) error {
	_, err := s.conn.WriteToUDP(payload, s.target)
	return err
}

func (s *sender) run(ctx context.Context, h *hub) error {
	go func() {
		<-ctx.Done()
		_ = s.conn.Close()
	}()

	buf := make([]byte, 65536)
	for {
		n, src, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("sender read: %w", err)
		}
		payload := append([]byte(nil), buf[:n]...)
		h.broadcast(newEvent("send", "rx", src.String(), payload))
	}
}

// nolint:cyclop
func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	httpAddr := utils.GetEnv("MOCK_HTTP_ADDR", ":8080")
	listenAddr := utils.GetEnv("MOCK_LISTEN_ADDR", ":27015")
	targetAddr := utils.GetEnv("MOCK_TARGET_ADDR", "127.0.0.1:27016")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	h := newHub()

	snd, err := newSender(targetAddr)
	if err != nil {
		slog.Error("sender init failed", "err", err)
		os.Exit(1)
	}
	slog.Info("sender bound", "local", snd.localAddr(), "target", snd.targetAddr())

	go func() {
		if err := runListener(ctx, listenAddr, h); err != nil {
			slog.Error("listener exited", "err", err)
			cancel()
		}
	}()

	go func() {
		if err := snd.run(ctx, h); err != nil {
			slog.Error("sender exited", "err", err)
			cancel()
		}
	}()

	indexTmpl := template.Must(template.New("index").Parse(indexHTML))

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data := struct {
			ListenAddr  string
			TargetAddr  string
			SenderLocal string
		}{listenAddr, snd.targetAddr(), snd.localAddr()}
		if err := indexTmpl.Execute(w, data); err != nil {
			slog.Warn("render index", "err", err)
		}
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// Flush headers immediately so EventSource fires onopen before any event arrives.
		_, _ = fmt.Fprint(w, ":\n\n")
		flusher.Flush()

		ch := h.subscribe()
		defer h.unsubscribe(ch)

		for {
			select {
			case <-r.Context().Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				data, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Payload string `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		payload := []byte(req.Payload)
		if err := snd.send(payload); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.broadcast(newEvent("send", "tx", snd.targetAddr(), payload))
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{
		Addr:              httpAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("http server listening", "addr", httpAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server", "err", err)
		os.Exit(1)
	}
}
