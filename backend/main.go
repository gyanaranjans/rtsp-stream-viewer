package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/gyanaranjans/rtsp-stream-viewer/backend/internal/stream"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mgr := stream.NewManager(stream.Config{
		FFmpegPath:   env("FFMPEG_PATH", "ffmpeg"),
		MaxStreams:   envInt("MAX_STREAMS", 16),
		IdleTimeout:  time.Duration(envInt("IDLE_TIMEOUT_SEC", 10)) * time.Second,
		StallTimeout: time.Duration(envInt("STALL_TIMEOUT_SEC", 15)) * time.Second,
		SubBuffer:    64,
	}, log)

	srv := &http.Server{
		Addr:              ":" + env("PORT", "8080"),
		Handler:           newMux(mgr, log, os.Getenv("STATIC_DIR"), parseOrigins(os.Getenv("ALLOWED_ORIGINS"))),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	mgr.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func newMux(mgr *stream.Manager, log *slog.Logger, staticDir string, origins []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /api/streams", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mgr.Stats())
	})
	mux.HandleFunc("GET /ws", wsHandler(mgr, log, origins))
	if staticDir != "" {
		mux.Handle("/", spa(staticDir))
	}
	return cors(mux)
}

func wsHandler(mgr *stream.Manager, log *slog.Logger, origins []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		src := r.URL.Query().Get("url")
		if err := validateURL(src); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: origins})
		if err != nil {
			return
		}
		defer c.CloseNow()
		// Clients only receive; CloseRead handles pings/close frames and
		// cancels ctx when the peer goes away.
		ctx := c.CloseRead(r.Context())

		sub, err := mgr.Subscribe(src)
		if err != nil {
			writeJSON(ctx, c, stream.Event{Type: "error", Message: err.Error()})
			c.Close(websocket.StatusTryAgainLater, "stream limit reached")
			return
		}
		defer sub.Close()

		for {
			select {
			case <-ctx.Done():
				return
			case <-sub.Done:
				writeJSON(ctx, c, stream.Event{Type: "error", Message: sub.Reason})
				c.Close(websocket.StatusTryAgainLater, "dropped")
				return
			case msg := <-sub.C:
				var err error
				if msg.Event != nil {
					err = writeJSON(ctx, c, *msg.Event)
				} else {
					wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
					err = c.Write(wctx, websocket.MessageBinary, msg.Data)
					cancel()
				}
				if err != nil {
					return
				}
			}
		}
	}
}

func writeJSON(ctx context.Context, c *websocket.Conn, ev stream.Event) error {
	b, _ := json.Marshal(ev)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, b)
}

func validateURL(raw string) error {
	if raw == "" {
		return errors.New("missing url parameter")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("invalid url")
	}
	if u.Scheme != "rtsp" && u.Scheme != "rtsps" {
		return errors.New("url must use rtsp:// or rtsps://")
	}
	if u.Host == "" {
		return errors.New("url is missing a host")
	}
	return nil
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}

// spa serves the built frontend, falling back to index.html for client routes.
func spa(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := os.Stat(dir + r.URL.Path); err != nil {
			http.ServeFile(w, r, dir+"/index.html")
			return
		}
		fs.ServeHTTP(w, r)
	})
}

func parseOrigins(s string) []string {
	if s == "" {
		return []string{"*"}
	}
	return strings.Split(s, ",")
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}
