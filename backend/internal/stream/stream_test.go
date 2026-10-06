package stream

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:], typ)
	return append(b, payload...)
}

func TestReadBox(t *testing.T) {
	data := append(box("ftyp", []byte("isom")), box("mdat", []byte{1, 2, 3})...)
	r := bytes.NewReader(data)
	b1, err := readBox(r)
	if err != nil || b1.Type != "ftyp" || len(b1.Data) != 12 {
		t.Fatalf("got %v %v", b1, err)
	}
	b2, err := readBox(r)
	if err != nil || b2.Type != "mdat" || !bytes.Equal(b2.Data[8:], []byte{1, 2, 3}) {
		t.Fatalf("got %v %v", b2, err)
	}
	if _, err := readBox(r); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestReadBoxInvalidSize(t *testing.T) {
	bad := []byte{0, 0, 0, 4, 'm', 'o', 'o', 'f'}
	if _, err := readBox(bytes.NewReader(bad)); err == nil {
		t.Fatal("expected error for undersized box")
	}
}

func TestCodecString(t *testing.T) {
	moov := box("moov", box("avcC", []byte{1, 0x64, 0x00, 0x1f, 0xff}))
	c, err := codecString(moov)
	if err != nil || c != "avc1.64001f" {
		t.Fatalf("got %q %v", c, err)
	}
	if _, err := codecString(box("moov", box("hvcC", []byte{1, 2, 3, 4}))); err != errNoCodec {
		t.Fatalf("want errNoCodec, got %v", err)
	}
}

func TestFriendlyError(t *testing.T) {
	cases := map[string]string{
		"rtsp://x/y: Server returned 404 Not Found":            "stream not found on server (404)",
		"Connection to tcp://h:554 failed: Connection refused": "connection refused — is the RTSP server running?",
		"something odd": "something odd",
	}
	for in, want := range cases {
		if got := friendlyError(in); got != want {
			t.Errorf("friendlyError(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTailBufferStripsPrefix(t *testing.T) {
	tb := &tailBuffer{max: 4}
	tb.Write([]byte("[rtsp @ 0x123] method DESCRIBE failed: 404 Not Found\nError opening input files: Server returned 404\n"))
	if got := tb.Last(); got != "Error opening input files: Server returned 404" && got != "method DESCRIBE failed: 404 Not Found" {
		t.Fatalf("unexpected %q", got)
	}
}

func TestRedact(t *testing.T) {
	if got := redact("rtsp://user:pw@cam:554/s"); got != "rtsp://***@cam:554/s" {
		t.Fatal(got)
	}
	if got := redact("rtsp://cam/s"); got != "rtsp://cam/s" {
		t.Fatal(got)
	}
}

// Integration tests: run real FFmpeg against generated files.

func makeClip(t *testing.T, name string, codecArgs ...string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(t.TempDir(), name)
	args := append([]string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i",
		"testsrc=size=320x240:rate=25", "-t", "3"}, codecArgs...)
	args = append(args, "-g", "25", "-pix_fmt", "yuv420p", out)
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("cannot create clip: %v %s", err, b)
	}
	return out
}

func testManager() *Manager {
	return NewManager(Config{FFmpegPath: "ffmpeg", MaxStreams: 2, IdleTimeout: 100 * time.Millisecond,
		StallTimeout: 5 * time.Second, SubBuffer: 256}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// collect reads messages until it has seen an init event and n fragments.
func collect(t *testing.T, sub *Subscriber, n int) (codec string, frags int) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	gotInitData := false
	for frags < n {
		select {
		case m := <-sub.C:
			switch {
			case m.Event != nil && m.Event.Type == "init":
				codec = m.Event.Codec
			case m.Data != nil && !gotInitData:
				if string(m.Data[4:8]) != "ftyp" {
					t.Fatalf("first binary message should be init segment, got %q", m.Data[4:8])
				}
				gotInitData = true
			case m.Data != nil:
				if string(m.Data[4:8]) != "moof" {
					t.Fatalf("fragment should start with moof, got %q", m.Data[4:8])
				}
				frags++
			}
		case <-deadline:
			t.Fatalf("timeout: codec=%q frags=%d", codec, frags)
		}
	}
	return
}

func TestStreamH264Copy(t *testing.T) {
	clip := makeClip(t, "h264.mp4", "-c:v", "libx264")
	m := testManager()
	defer m.Shutdown()
	sub, err := m.Subscribe(clip)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	codec, _ := collect(t, sub, 2)
	if !strings.HasPrefix(codec, "avc1.") {
		t.Fatalf("codec %q", codec)
	}
	if st := m.Stats(); len(st) != 1 || st[0].Transcoding {
		t.Fatalf("stats %+v", st)
	}
}

func TestStreamHEVCTranscodes(t *testing.T) {
	clip := makeClip(t, "hevc.mp4", "-c:v", "libx265", "-x265-params", "log-level=error")
	m := testManager()
	defer m.Shutdown()
	sub, err := m.Subscribe(clip)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	codec, _ := collect(t, sub, 1)
	if !strings.HasPrefix(codec, "avc1.") {
		t.Fatalf("codec %q", codec)
	}
	if st := m.Stats(); len(st) != 1 || !st[0].Transcoding {
		t.Fatalf("expected transcoding, stats %+v", st)
	}
}

func TestSharedStreamAndLimit(t *testing.T) {
	clip := makeClip(t, "h264.mp4", "-c:v", "libx264")
	m := testManager()
	defer m.Shutdown()
	a, _ := m.Subscribe(clip)
	b, _ := m.Subscribe(clip)
	defer a.Close()
	defer b.Close()
	if len(m.Stats()) != 1 || m.Stats()[0].Subscribers != 2 {
		t.Fatalf("expected one shared stream with 2 subs: %+v", m.Stats())
	}
	c, _ := m.Subscribe("rtsp://127.0.0.1:1/nothing")
	defer c.Close()
	if _, err := m.Subscribe("rtsp://127.0.0.1:1/other"); err != ErrTooManyStreams {
		t.Fatalf("want ErrTooManyStreams, got %v", err)
	}
}

func TestConnectionErrorReported(t *testing.T) {
	m := testManager()
	defer m.Shutdown()
	sub, _ := m.Subscribe("rtsp://127.0.0.1:1/nothing")
	defer sub.Close()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case msg := <-sub.C:
			if msg.Event != nil && msg.Event.State == StateReconnecting {
				if !strings.Contains(msg.Event.Message, "refused") {
					t.Fatalf("unexpected message %q", msg.Event.Message)
				}
				return
			}
		case <-deadline:
			t.Fatal("no reconnecting status")
		}
	}
}

func TestIdleStreamStops(t *testing.T) {
	clip := makeClip(t, "h264.mp4", "-c:v", "libx264")
	m := testManager()
	sub, _ := m.Subscribe(clip)
	sub.Close()
	time.Sleep(500 * time.Millisecond)
	if n := len(m.Stats()); n != 0 {
		t.Fatalf("expected idle stream removed, have %d", n)
	}
}
