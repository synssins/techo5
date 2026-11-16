package agenthub

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/mic"
)

// What goes up is a WAV AgentHub reads: mono, 16-bit, at the microphone's rate, the samples as they were.
func TestWAV(t *testing.T) {
	b := wav([]int16{1, -2, 3})
	if string(b[0:4]) != "RIFF" || string(b[8:16]) != "WAVEfmt " || string(b[36:40]) != "data" || len(b) != 44+6 {
		t.Fatalf("not a WAV: % x", b)
	}
	le := binary.LittleEndian
	if le.Uint16(b[22:]) != 1 || le.Uint32(b[24:]) != mic.Rate || le.Uint16(b[34:]) != 16 || le.Uint32(b[40:]) != 6 {
		t.Fatalf("format: channels %d rate %d bits %d data %d", le.Uint16(b[22:]), le.Uint32(b[24:]), le.Uint16(b[34:]), le.Uint32(b[40:]))
	}
	if int16(le.Uint16(b[46:])) != -2 {
		t.Fatalf("samples changed: % x", b[44:])
	}
}

// The chat model can be chosen only once speech to text, text to speech and its address are set, and
// emptying one of them while it is chosen goes back to Home Assistant.
func TestTheChatModelNeedsAllThree(t *testing.T) {
	config.Use(filepath.Join(t.TempDir(), "state.json"))
	f := newFeature()
	f.brain.OnCommand(brainDirect)
	if config.Get().Brain.Mode != config.BrainHomeAssistant {
		t.Fatal("chosen with nothing set")
	}
	f.stt.OnCommand("192.168.1.86:10300")
	f.tts.OnCommand("192.168.1.86:10200")
	f.llm.OnCommand("not an address")
	f.brain.OnCommand(brainDirect)
	if b := config.Get().Brain; b.Mode != config.BrainHomeAssistant || b.LLM != "" {
		t.Fatalf("a bad address was kept or chosen: %+v", b)
	}
	f.llm.OnCommand("http://192.168.1.86:8787/v1/")
	f.brain.OnCommand(brainDirect)
	if b := config.Get().Brain; b.Mode != config.BrainDirect || b.LLM != "http://192.168.1.86:8787/v1" {
		t.Fatalf("not chosen with all three set: %+v", b)
	}
	f.tts.OnCommand("")
	if config.Get().Brain.Mode != config.BrainHomeAssistant {
		t.Fatal("still answering directly with no text to speech")
	}
}

// A key typed in Home Assistant is saved; sending back what the text shows for it keeps it, and an
// empty one removes it.
func TestKeys(t *testing.T) {
	config.Use(filepath.Join(t.TempDir(), "state.json"))
	f := newFeature()
	f.hubKey.OnCommand("secret-1")
	if config.Get().AgentHub.Key != "secret-1" {
		t.Fatal("the key was not saved")
	}
	f.hubKey.OnCommand(keySet)
	if config.Get().AgentHub.Key != "secret-1" {
		t.Fatal("the shown placeholder replaced the key")
	}
	f.hubKey.OnCommand("")
	if config.Get().AgentHub.Key != "" {
		t.Fatal("an empty key did not remove it")
	}
	f.replies.OnCommand("media_player.office_ceiling")
	if config.Get().Brain.ReplyPlayer != "media_player.office_ceiling" {
		t.Fatal("replies play on was not saved")
	}
	f.replies.OnCommand("not a player")
	if config.Get().Brain.ReplyPlayer != "media_player.office_ceiling" {
		t.Fatal("a value that is not a media player replaced it")
	}
	f.brainKey.OnCommand("k2")
	if config.Get().Brain.Key != "k2" {
		t.Fatal("the chat model key was not saved")
	}
}

// A reply's address relays it from AgentHub with the key, as it comes, counts what went (for a player
// elsewhere, which cannot be heard finishing), and answers once.
func TestRelay(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" || r.URL.Path != "/api/device/audio/9" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		hdr := wav(nil)
		binary.LittleEndian.PutUint32(hdr[24:], 24000)
		_, _ = w.Write(hdr)
		_, _ = w.Write(make([]byte, 48000)) // one second at 24 kHz
	}))
	defer hub.Close()
	l := &loop{served: map[string]*relay{}}
	u, r, ok := l.offer(config.AgentHub{URL: hub.URL, Key: "k"}, "/api/device/audio/9", "127.0.0.1")
	if !ok || !l.serving() {
		t.Fatal("nothing offered")
	}
	req := httptest.NewRequest(http.MethodGet, u, nil)
	rec := httptest.NewRecorder()
	l.serve(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "audio/wav" || rec.Body.Len() != 44+48000 {
		t.Fatalf("relayed %d %q %d bytes", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	select {
	case <-r.done:
	default:
		t.Fatal("not done after the whole reply went")
	}
	if r.rate != 24000 || r.bytes != 48000 {
		t.Fatalf("counted rate %d, %d bytes", r.rate, r.bytes)
	}
	rec = httptest.NewRecorder()
	l.serve(rec, httptest.NewRequest(http.MethodGet, u, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("answered twice: %d", rec.Code)
	}
}
