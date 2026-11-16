// Package agenthub is two ways of answering without Home Assistant's Assist pipeline, both set from the
// device's page in Home Assistant:
//
//   - The assistant's brain: the direct pipeline (feature/voice/direct.go, config.Brain), whose chat model,
//     speech to text and text to speech were only on the setup page before.
//   - Listening without a wake word (listen.go): every stretch of speech goes to an AgentHub server, which
//     keeps only what was said to one of its agents and sends the spoken answer back.
package agenthub

import (
	"log/slog"
	"net"
	"net/url"
	"strings"

	esphome "github.com/ygelfand/go-esphome-device"

	"github.com/HuskerMinion/techo5/echod/internal/component"
	"github.com/HuskerMinion/techo5/echod/internal/config"
)

func init() {
	component.Register(component.Device, Get())
}

const (
	brainHA     = "Home Assistant"
	brainDirect = "Chat model"

	// keySet is what a key's text shows once one is saved: the key itself never goes back to Home
	// Assistant, where it would sit in the state history.
	keySet = "(saved)"
)

// Feature is the settings' entities and the listening loop.
type Feature struct {
	brain    *esphome.Select
	stt      *esphome.Text
	tts      *esphome.Text
	llm      *esphome.Text
	model    *esphome.Text
	brainKey *esphome.Text
	replies  *esphome.Text

	listen *esphome.Switch
	hubURL *esphome.Text
	hubKey *esphome.Text
	status *esphome.TextSensor

	loop *loop
}

var shared = newFeature()

// Get is the feature.
func Get() *Feature { return shared }

func newFeature() *Feature {
	cfg := func(id, name, icon string) esphome.Base {
		return esphome.Base{ObjectID: id, Name: name, Icon: icon, Category: esphome.CategoryConfig}
	}
	text := func(id, name, icon string, max uint32) *esphome.Text {
		return &esphome.Text{Base: cfg(id, name, icon), MaxLength: max}
	}
	secret := func(id, name string) *esphome.Text {
		return &esphome.Text{Base: cfg(id, name, "mdi:key"), MaxLength: 255, Mode: esphome.TextPassword}
	}
	f := &Feature{
		brain:    &esphome.Select{Base: cfg("brain", "Answered by", "mdi:head-cog"), Options: []string{brainHA, brainDirect}},
		stt:      text("brain_stt", "Chat model speech to text", "mdi:microphone-message", 100),
		tts:      text("brain_tts", "Chat model text to speech", "mdi:account-voice", 100),
		llm:      text("brain_llm", "Chat model address", "mdi:server-network", 255),
		model:    text("brain_model", "Chat model name", "mdi:tag-text", 100),
		brainKey: secret("brain_key", "Chat model key"),
		replies:  text("replies_play_on", "Replies play on", "mdi:speaker-wireless", 255),
		listen:   &esphome.Switch{Base: cfg("agenthub_listen", "Listen without a wake word", "mdi:ear-hearing")},
		hubURL:   text("agenthub_url", "AgentHub address", "mdi:server-network", 255),
		hubKey:   secret("agenthub_key", "AgentHub device key"),
		status: &esphome.TextSensor{Base: esphome.Base{ObjectID: "agenthub_status", Name: "AgentHub listening",
			Icon: "mdi:ear-hearing", Category: esphome.CategoryDiagnostic}},
	}
	f.loop = newLoop(f.status.Set)

	f.brain.OnCommand = f.setBrain
	f.stt.OnCommand = func(v string) { f.setHostPort(f.stt, v, func(b *config.Brain) *string { return &b.STT }) }
	f.tts.OnCommand = func(v string) { f.setHostPort(f.tts, v, func(b *config.Brain) *string { return &b.TTS }) }
	f.llm.OnCommand = f.setLLM
	f.model.OnCommand = func(v string) {
		f.setBrainField(f.model, strings.TrimSpace(v), func(b *config.Brain) *string { return &b.Model })
	}
	f.brainKey.OnCommand = func(v string) {
		f.setKey(f.brainKey, v, config.Get().Brain.Key, config.Set().Brain().SetKey)
	}
	f.replies.OnCommand = func(v string) {
		if err := config.Set().Brain().SetReplyPlayer(v); err != nil {
			slog.Warn("agenthub: replies play on", "err", err)
		}
		f.replies.Set(config.Get().Brain.ReplyPlayer)
	}
	f.listen.OnCommand = func(on bool) {
		if err := config.Set().AgentHub().On(on); err != nil {
			slog.Error("agenthub: saving the switch failed", "err", err)
			return
		}
		f.listen.Set(on)
		f.loop.changed()
	}
	f.hubURL.OnCommand = func(v string) {
		if err := config.Set().AgentHub().URL(v); err != nil {
			slog.Warn("agenthub: address not kept", "err", err)
		}
		f.hubURL.Set(config.Get().AgentHub.URL)
		f.loop.changed()
	}
	f.hubKey.OnCommand = func(v string) {
		f.setKey(f.hubKey, v, config.Get().AgentHub.Key, config.Set().AgentHub().Key)
		f.loop.changed()
	}
	return f
}

func (f *Feature) Name() string { return "agenthub" }

func (f *Feature) Entities() []esphome.Entity {
	return []esphome.Entity{f.brain, f.stt, f.tts, f.llm, f.model, f.brainKey, f.replies, f.listen, f.hubURL, f.hubKey, f.status}
}

// Restore shows the saved settings, keys only as saved or not.
func (f *Feature) Restore(c config.Config) {
	b := c.Brain
	f.brain.Set(brainOption(b.Mode))
	f.stt.Set(b.STT)
	f.tts.Set(b.TTS)
	f.llm.Set(b.LLM)
	f.model.Set(b.Model)
	f.brainKey.Set(masked(b.Key))
	f.replies.Set(b.ReplyPlayer)
	f.listen.Set(c.AgentHub.On)
	f.hubURL.Set(c.AgentHub.URL)
	f.hubKey.Set(masked(c.AgentHub.Key))
	f.loop.changed()
}

func brainOption(m config.BrainMode) string {
	if m == config.BrainDirect {
		return brainDirect
	}
	return brainHA
}

func masked(key string) string {
	if key == "" {
		return ""
	}
	return keySet
}

// setBrain chooses Home Assistant or the chat model. The chat model needs all three of speech to text,
// text to speech and its address first, as the setup page asks: chosen without them, the next wake
// word would have nowhere to go.
func (f *Feature) setBrain(opt string) {
	b := config.Get().Brain
	switch opt {
	case brainHA:
		b.Mode = config.BrainHomeAssistant
	case brainDirect:
		b.Mode = config.BrainDirect
		if !b.Direct() {
			slog.Warn("agenthub: the chat model needs speech to text, text to speech and its address set first")
			f.brain.Set(brainOption(config.Get().Brain.Mode))
			return
		}
	default:
		return
	}
	if err := config.Set().Brain().Set(b); err != nil {
		slog.Error("agenthub: saving the brain failed", "err", err)
	}
	f.brain.Set(brainOption(config.Get().Brain.Mode))
	slog.Info("assistant brain chosen", "mode", b.Mode)
}

// setHostPort keeps a Wyoming server's host:port, or empty.
func (f *Feature) setHostPort(t *esphome.Text, v string, field func(*config.Brain) *string) {
	v = strings.TrimSpace(v)
	if v != "" {
		if _, port, err := net.SplitHostPort(v); err != nil || port == "" {
			slog.Warn("agenthub: should be host:port, like 192.168.1.86:10300", "got", v)
			b := config.Get().Brain
			t.Set(*field(&b))
			return
		}
	}
	f.setBrainField(t, v, field)
}

// setLLM keeps the chat model's address: http(s) with a host, or empty.
func (f *Feature) setLLM(v string) {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	if v != "" {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			slog.Warn("agenthub: the chat model address should be like http://192.168.1.86:8787/v1", "got", v)
			f.llm.Set(config.Get().Brain.LLM)
			return
		}
	}
	f.setBrainField(f.llm, v, func(b *config.Brain) *string { return &b.LLM })
}

// setBrainField saves one of the brain's settings. Emptying one the chat model needs while it is chosen
// goes back to Home Assistant, rather than leaving the next wake word nowhere to go.
func (f *Feature) setBrainField(t *esphome.Text, v string, field func(*config.Brain) *string) {
	b := config.Get().Brain
	*field(&b) = v
	if b.Mode == config.BrainDirect && !b.Direct() {
		b.Mode = config.BrainHomeAssistant
		slog.Warn("agenthub: a chat model setting was emptied, answering through Home Assistant again")
	}
	if err := config.Set().Brain().Set(b); err != nil {
		slog.Error("agenthub: saving the brain failed", "err", err)
	}
	b = config.Get().Brain
	t.Set(*field(&b))
	f.brain.Set(brainOption(b.Mode))
}

// setKey saves a key typed in Home Assistant. Sending back what the text shows for a saved key keeps the
// key; an empty one removes it.
func (f *Feature) setKey(t *esphome.Text, v, have string, save func(string) error) {
	v = strings.TrimSpace(v)
	if v == keySet {
		t.Set(masked(have))
		return
	}
	if err := save(v); err != nil {
		slog.Warn("agenthub: key not kept", "err", err)
		t.Set(masked(have))
		return
	}
	t.Set(masked(v))
}
