package sidecar

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	sxm "github.com/codester28351/siriusxm-sidecar/sxm"
)

type Station struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type NowPlaying struct {
	Title  string `json:"title,omitempty"`
	Artist string `json:"artist,omitempty"`
	Album  string `json:"album,omitempty"`
}

type SiriusProvider struct {
	client    *sxm.SiriusXM
	hlsHost   string
	hlsPort   int
	ffmpeg    string
	mu        sync.Mutex
	hlsServer *http.Server
	hlsErr    chan error
}

func NewSiriusProvider(username, password string) *SiriusProvider {
	host := envOr("SXM_HLS_HOST", "127.0.0.1")
	port := 9998
	if raw := strings.TrimSpace(os.Getenv("SXM_HLS_PORT")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n < 65536 {
			port = n
		}
	}
	ff := envOr("FFMPEG", "ffmpeg")
	return &SiriusProvider{client: sxm.New(username, password), hlsHost: host, hlsPort: port, ffmpeg: ff, hlsErr: make(chan error, 1)}
}

func (p *SiriusProvider) Start(ctx context.Context) error {
	if _, err := exec.LookPath(p.ffmpeg); err != nil {
		return fmt.Errorf("FFmpeg not found (%q); install FFmpeg or set FFMPEG: %w", p.ffmpeg, err)
	}
	if p.hlsHost != "127.0.0.1" && p.hlsHost != "localhost" {
	}
	mux := http.NewServeMux()
	mux.Handle("/", p.client)
	addr := p.hlsHost + ":" + strconv.Itoa(p.hlsPort)
	p.hlsServer = &http.Server{Addr: addr, Handler: mux}
	go func() {
		err := p.hlsServer.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			p.hlsErr <- err
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := http.Get("http://" + addr + "/server-mode")
		if err == nil {
			conn.Body.Close()
			return nil
		}
		select {
		case err := <-p.hlsErr:
			return fmt.Errorf("start internal HLS server: %w", err)
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("internal HLS server did not start on %s", addr)
}

func (p *SiriusProvider) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hlsServer != nil {
		_ = p.hlsServer.Shutdown(context.Background())
		p.hlsServer = nil
	}
}

func (p *SiriusProvider) Stations() []Station {
	channels := p.client.GetChannels()
	out := make([]Station, 0, len(channels))
	for _, c := range channels {
		if c.ChannelID == "" || c.Name == "" {
			continue
		}
		out = append(out, Station{ID: c.ChannelID, Name: c.Name})
	}
	return out
}

func (p *SiriusProvider) channelName(id string) (string, bool) {
	_, cid, name, _, _ := p.client.GetChannel(id)
	return name, cid != ""
}

func (p *SiriusProvider) NowPlaying(id string) (NowPlaying, error) {
	name, ok := p.channelName(id)
	if !ok {
		return NowPlaying{}, fmt.Errorf("station %q not found", id)
	}
	playlist := p.client.GetPlaylist(name, true)
	if playlist == "" {
		return NowPlaying{}, fmt.Errorf("now-playing data unavailable for %q", id)
	}
	title := ""
	for _, line := range strings.Split(playlist, "\n") {
		if strings.HasPrefix(line, "#EXTINF:") {
			if i := strings.IndexByte(line, ','); i >= 0 {
				title = strings.TrimSpace(line[i+1:])
			}
		}
	}
	artist, song := splitArtistTitle(title)
	return NowPlaying{Title: song, Artist: artist}, nil
}

func splitArtistTitle(v string) (string, string) {
	if i := strings.Index(v, " - "); i >= 0 {
		return strings.TrimSpace(v[:i]), strings.TrimSpace(v[i+3:])
	}
	return "", strings.TrimSpace(v)
}

func (p *SiriusProvider) Stream(id string, w http.ResponseWriter, r *http.Request) error {
	name, ok := p.channelName(id)
	if !ok {
		return fmt.Errorf("station %q not found", id)
	}
	if p.client.GetPlaylist(name, true) == "" {
		return fmt.Errorf("playlist unavailable for %q", id)
	}
	src := fmt.Sprintf("http://%s:%d/%s.m3u8", p.hlsHost, p.hlsPort, id)
	args := []string{
		"-hide_banner", "-loglevel", "warning",
		"-live_start_index", "-3", "-http_persistent", "1",
		"-i", src, "-vn", "-ac", "2", "-ar", "44100",
		"-c:a", "libmp3lame", "-b:a", "256k", "-f", "mp3", "pipe:1",
	}
	cmd := exec.CommandContext(r.Context(), p.ffmpeg, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start FFmpeg: %w", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	go func() {
		b, _ := io.ReadAll(stderr)
		if len(b) > 0 {
			fmt.Printf("ffmpeg[%s]: %s", id, b)
		}
	}()
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	buf := make([]byte, 64*1024)
	for {
		n, e := stdout.Read(buf)
		if n > 0 {
			if _, we := w.Write(buf[:n]); we != nil {
				return nil
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if e != nil {
			if e == io.EOF {
				return nil
			}
			return e
		}
	}
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
