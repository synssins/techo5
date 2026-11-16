package agenthub

import (
	"encoding/binary"
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
	f.brainKey.OnCommand("k2")
	if config.Get().Brain.Key != "k2" {
		t.Fatal("the chat model key was not saved")
	}
}
