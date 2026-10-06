package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gyanaranjans/rtsp-stream-viewer/backend/internal/stream"
)

func TestValidateURL(t *testing.T) {
	ok := []string{"rtsp://cam/s", "rtsps://user:pw@cam:322/live"}
	bad := []string{"", "http://cam/s", "file:///etc/passwd", "rtsp://", "-i foo"}
	for _, u := range ok {
		if err := validateURL(u); err != nil {
			t.Errorf("%q: %v", u, err)
		}
	}
	for _, u := range bad {
		if validateURL(u) == nil {
			t.Errorf("%q should be rejected", u)
		}
	}
}

func TestWSRejectsBadURL(t *testing.T) {
	mgr := stream.NewManager(stream.Config{MaxStreams: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(newMux(mgr, slog.Default(), "", []string{"*"}))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/ws?url=http://x")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", res.StatusCode)
	}
}
