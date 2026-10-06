// Package stream turns RTSP sources into fragmented-MP4 byte streams via
// FFmpeg and fans each source out to any number of subscribers.
package stream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Message is delivered to subscribers. Exactly one of Event or Data is set.
type Message struct {
	Event *Event
	Data  []byte
}

// Event is a JSON-serialisable control message for the client.
type Event struct {
	Type    string `json:"type"` // "status" | "init"
	State   string `json:"state,omitempty"`
	Message string `json:"message,omitempty"`
	Codec   string `json:"codec,omitempty"`
}

const (
	StateConnecting   = "connecting"
	StateLive         = "live"
	StateReconnecting = "reconnecting"
)

type Config struct {
	FFmpegPath   string
	MaxStreams   int
	IdleTimeout  time.Duration // how long a stream keeps running with no subscribers
	StallTimeout time.Duration // restart FFmpeg if no media arrives for this long
	SubBuffer    int           // per-subscriber queue length (in fragments)
}

var ErrTooManyStreams = errors.New("server is at its concurrent stream limit")

type Manager struct {
	cfg     Config
	log     *slog.Logger
	mu      sync.Mutex
	streams map[string]*Stream
}

func NewManager(cfg Config, log *slog.Logger) *Manager {
	return &Manager{cfg: cfg, log: log, streams: map[string]*Stream{}}
}

// Subscribe attaches to the stream for url, starting it if needed.
func (m *Manager) Subscribe(url string) (*Subscriber, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.streams[url]
	if !ok {
		if len(m.streams) >= m.cfg.MaxStreams {
			return nil, ErrTooManyStreams
		}
		s = newStream(url, m)
		m.streams[url] = s
		go s.run()
	}
	return s.add(), nil
}

type StreamInfo struct {
	URL         string `json:"url"`
	State       string `json:"state"`
	Codec       string `json:"codec,omitempty"`
	Transcoding bool   `json:"transcoding"`
	Subscribers int    `json:"subscribers"`
	Restarts    int    `json:"restarts"`
}

func (m *Manager) Stats() []StreamInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]StreamInfo, 0, len(m.streams))
	for _, s := range m.streams {
		out = append(out, s.info())
	}
	return out
}

// Shutdown stops every running stream.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.streams {
		s.cancel()
	}
}

func (m *Manager) remove(s *Stream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.streams[s.url] == s {
		delete(m.streams, s.url)
	}
}

type Subscriber struct {
	C      chan Message
	stream *Stream
	once   sync.Once
	// Closed when the stream drops this subscriber (slow consumer or shutdown).
	Done chan struct{}
	// Reason is set before Done is closed.
	Reason string
}

func (sub *Subscriber) Close() { sub.stream.removeSub(sub, "") }

type Stream struct {
	url    string
	m      *Manager
	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	subs       map[*Subscriber]struct{}
	init       []byte // ftyp+moov of the current FFmpeg run
	codec      string
	state      string
	lastStatus Event
	transcode  bool
	restarts   int
	idleTimer  *time.Timer
}

func newStream(url string, m *Manager) *Stream {
	ctx, cancel := context.WithCancel(context.Background())
	return &Stream{
		url: url, m: m, ctx: ctx, cancel: cancel,
		subs:       map[*Subscriber]struct{}{},
		state:      StateConnecting,
		lastStatus: Event{Type: "status", State: StateConnecting},
	}
}

func (s *Stream) info() StreamInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return StreamInfo{URL: redact(s.url), State: s.state, Codec: s.codec, Transcoding: s.transcode,
		Subscribers: len(s.subs), Restarts: s.restarts}
}

func (s *Stream) add() *Subscriber {
	sub := &Subscriber{C: make(chan Message, s.m.cfg.SubBuffer), stream: s, Done: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.subs[sub] = struct{}{}
	// Late joiners get current status and, if live, the init segment so they
	// can start decoding at the next keyframe-aligned fragment.
	st := s.lastStatus
	sub.C <- Message{Event: &st}
	if s.init != nil {
		sub.C <- Message{Event: &Event{Type: "init", Codec: s.codec}}
		sub.C <- Message{Data: s.init}
	}
	return sub
}

func (s *Stream) removeSub(sub *Subscriber, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropLocked(sub, reason)
}

func (s *Stream) dropLocked(sub *Subscriber, reason string) {
	if _, ok := s.subs[sub]; !ok {
		return
	}
	delete(s.subs, sub)
	sub.once.Do(func() {
		sub.Reason = reason
		close(sub.Done)
	})
	if len(s.subs) == 0 && s.idleTimer == nil {
		s.idleTimer = time.AfterFunc(s.m.cfg.IdleTimeout, s.stopIfIdle)
	}
}

func (s *Stream) stopIfIdle() {
	s.mu.Lock()
	idle := len(s.subs) == 0
	s.mu.Unlock()
	if idle {
		s.m.log.Info("stopping idle stream", "url", redact(s.url))
		s.m.remove(s)
		s.cancel()
	}
}

// broadcast sends msg to all subscribers. Subscribers whose queue is full are
// dropped rather than blocking the pipeline for everyone else; the client
// reconnects and resumes from the next keyframe.
func (s *Stream) broadcast(msg Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broadcastLocked(msg)
}

func (s *Stream) broadcastLocked(msg Message) {
	for sub := range s.subs {
		select {
		case sub.C <- msg:
		default:
			s.dropLocked(sub, "client too slow to keep up with stream")
		}
	}
}

func (s *Stream) setStatus(state, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	s.lastStatus = Event{Type: "status", State: state, Message: message}
	ev := s.lastStatus
	s.broadcastLocked(Message{Event: &ev})
}

func (s *Stream) run() {
	defer s.m.remove(s)
	backoff := time.Second
	for {
		started := time.Now()
		err := s.runFFmpeg()
		if s.ctx.Err() != nil {
			return
		}
		if errors.Is(err, errNoCodec) {
			s.mu.Lock()
			already := s.transcode
			s.transcode = true
			s.mu.Unlock()
			if !already {
				s.m.log.Info("source is not H.264, switching to transcoding", "url", redact(s.url))
				s.setStatus(StateConnecting, "Source codec not browser-compatible, transcoding to H.264")
				continue
			}
		}
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		}
		msg := "stream ended"
		if err != nil {
			msg = err.Error()
		}
		s.mu.Lock()
		s.restarts++
		s.init = nil
		s.mu.Unlock()
		s.m.log.Warn("ffmpeg exited", "url", redact(s.url), "err", msg, "retry_in", backoff)
		s.setStatus(StateReconnecting, fmt.Sprintf("%s — retrying in %s", msg, backoff))
		select {
		case <-time.After(backoff):
		case <-s.ctx.Done():
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (s *Stream) ffmpegArgs() []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if strings.HasPrefix(s.url, "rtsp") {
		args = append(args, "-rtsp_transport", "tcp", "-timeout", "10000000")
	}
	args = append(args, "-fflags", "nobuffer", "-i", s.url, "-an", "-sn", "-dn", "-map", "0:v:0")
	s.mu.Lock()
	transcode := s.transcode
	s.mu.Unlock()
	if transcode {
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency",
			"-profile:v", "main", "-pix_fmt", "yuv420p", "-g", "50", "-keyint_min", "25",
			"-vf", "scale='min(1280,iw)':-2")
	} else {
		args = append(args, "-c:v", "copy")
	}
	return append(args, "-f", "mp4",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof", "pipe:1")
}

// runFFmpeg runs one FFmpeg process until it exits, the stream is cancelled,
// or it stalls.
func (s *Stream) runFFmpeg() error {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.m.cfg.FFmpegPath, s.ffmpegArgs()...)
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &tailBuffer{max: 8}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ffmpeg: %w", err)
	}

	alive := make(chan struct{}, 1)
	go func() { // stall watchdog
		t := time.NewTimer(s.m.cfg.StallTimeout)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-alive:
				t.Reset(s.m.cfg.StallTimeout)
			case <-t.C:
				stderr.Write([]byte("no video data received (stream stalled)\n"))
				cancel()
				return
			}
		}
	}()

	readErr := s.pump(bufio.NewReaderSize(stdout, 256<<10), alive)
	if readErr != nil {
		cancel() // kill ffmpeg if we bailed out early (e.g. unsupported codec)
	}
	waitErr := cmd.Wait()
	if errors.Is(readErr, errNoCodec) {
		return readErr
	}
	if msg, ok := friendlyError(stderr.All()); ok {
		return errors.New(msg)
	}
	if line := stderr.Last(); line != "" {
		// Raw FFmpeg output often echoes the input URL; never leak its credentials.
		return errors.New(strings.ReplaceAll(line, s.url, redact(s.url)))
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return readErr
	}
	return waitErr
}

func (s *Stream) pump(r io.Reader, alive chan<- struct{}) error {
	var init []byte
	var frag []byte
	for {
		box, err := readBox(r)
		if err != nil {
			return err
		}
		select {
		case alive <- struct{}{}:
		default:
		}
		switch box.Type {
		case "ftyp":
			init = append(init[:0], box.Data...)
		case "moov":
			init = append(init, box.Data...)
			codec, err := codecString(box.Data)
			if err != nil {
				return err
			}
			s.mu.Lock()
			s.init, s.codec, s.state = init, codec, StateLive
			s.lastStatus = Event{Type: "status", State: StateLive}
			live := s.lastStatus
			s.broadcastLocked(Message{Event: &Event{Type: "init", Codec: codec}})
			s.broadcastLocked(Message{Data: init})
			s.broadcastLocked(Message{Event: &live})
			s.mu.Unlock()
		case "moof":
			frag = box.Data
		case "mdat":
			if frag == nil {
				continue
			}
			// One message per fragment keeps moof/mdat pairs atomic for MSE.
			msg := make([]byte, 0, len(frag)+len(box.Data))
			msg = append(append(msg, frag...), box.Data...)
			frag = nil
			s.broadcast(Message{Data: msg})
		}
	}
}

// friendlyError maps common FFmpeg failure output to something a user can act on.
func friendlyError(output string) (string, bool) {
	l := strings.ToLower(output)
	switch {
	case strings.Contains(l, "no route to host") || strings.Contains(l, "name or service not known") ||
		strings.Contains(l, "nodename nor servname") || strings.Contains(l, "failed to resolve"):
		return "host unreachable or could not be resolved — check the hostname", true
	case strings.Contains(l, "404") || strings.Contains(l, "not found"):
		return "stream not found on server (404)", true
	case strings.Contains(l, "400 bad request"):
		return "server rejected the request (400) — check the stream path", true
	case strings.Contains(l, "401") || strings.Contains(l, "unauthorized"):
		return "authentication failed (401) — check credentials in the URL", true
	case strings.Contains(l, "connection refused"):
		return "connection refused — is the RTSP server running?", true
	case strings.Contains(l, "timed out") || strings.Contains(l, "timeout"):
		return "connection timed out", true
	}
	return "", false
}

// tailBuffer keeps the last few non-empty lines written to it.
type tailBuffer struct {
	mu    sync.Mutex
	max   int
	lines []string
	part  string
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	parts := strings.Split(t.part+string(p), "\n")
	t.part = parts[len(parts)-1]
	for _, l := range parts[:len(parts)-1] {
		if l = strings.TrimSpace(l); l != "" {
			t.lines = append(t.lines, l)
		}
	}
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
	return len(p), nil
}

// All returns every retained line, for pattern matching.
func (t *tailBuffer) All() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n") + "\n" + t.part
}

// Last returns the most informative recent line, preferring the error cause
// over FFmpeg's generic trailing "Error opening input files" style lines.
func (t *tailBuffer) Last() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.lines) - 1; i >= 0; i-- {
		l := t.lines[i]
		if !strings.HasPrefix(l, "Error opening input") && !strings.HasPrefix(l, "Last message repeated") {
			return stripPrefix(l)
		}
	}
	if len(t.lines) > 0 {
		return stripPrefix(t.lines[len(t.lines)-1])
	}
	return strings.TrimSpace(t.part)
}

// stripPrefix removes FFmpeg's "[rtsp @ 0x...] " context prefix.
func stripPrefix(l string) string {
	if strings.HasPrefix(l, "[") {
		if i := strings.Index(l, "] "); i > 0 {
			return l[i+2:]
		}
	}
	return l
}

// redact hides credentials embedded in RTSP URLs for logging.
func redact(u string) string {
	if at := strings.LastIndex(u, "@"); at > 0 {
		if i := strings.Index(u, "://"); i > 0 && i < at {
			return u[:i+3] + "***@" + u[at+1:]
		}
	}
	return u
}
