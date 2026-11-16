package voice

import (
	"context"
	"sync"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/lib/safe"
)

// The AgentHub backend: a turn answered by an AgentHub server's agent rather than Home Assistant's
// pipeline (config.Brain chosen as AgentHub). What was said goes up whole once the speaker has finished,
// as the direct pipeline sends it; AgentHub transcribes it, its agent answers, and the reply's address
// comes back to play like Home Assistant's, here or on the player replies play on.

// HubTurn answers one utterance (16 kHz mono) through AgentHub: the reply's words and an address its audio
// plays from, or both empty when AgentHub kept nothing of it. Set by feature/agenthub; nil until it is.
var HubTurn func(ctx context.Context, pcm []int16) (reply, url string, err error)

// SetHubTurn installs it.
func SetHubTurn(fn func(ctx context.Context, pcm []int16) (string, string, error)) { HubTurn = fn }

// hubReady is whether a turn goes to AgentHub: chosen, with an address and a key, and installed.
func hubReady() bool {
	c := config.Get()
	return c.Brain.Mode == config.BrainAgentHub && c.AgentHub.URL != "" && c.AgentHub.Key != "" && HubTurn != nil
}

type hub struct {
	post func(event)

	mu     sync.Mutex
	pcm    []byte
	ctx    context.Context
	cancel context.CancelFunc
}

func newHub(post func(event)) *hub { return &hub{post: post} }

func (h *hub) Name() string { return "agenthub" }

func (h *hub) Ready() bool { return hubReady() }

func (h *hub) Start(string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel != nil {
		h.cancel()
	}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.pcm = h.pcm[:0]
	return nil
}

func (h *hub) Audio(frame []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx == nil || len(h.pcm)+len(frame) > maxUtterance {
		return nil
	}
	h.pcm = append(h.pcm, frame...)
	return nil
}

// End hands the utterance over, off the send queue, as the direct pipeline does.
func (h *hub) End() error {
	h.mu.Lock()
	ctx := h.ctx
	pcm := append([]byte(nil), h.pcm...)
	h.pcm = h.pcm[:0]
	h.mu.Unlock()
	if ctx == nil {
		return nil
	}
	safe.Go("agenthub turn", func() { h.answer(ctx, pcm) })
	return nil
}

func (h *hub) Stop() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel != nil {
		h.cancel()
	}
	h.ctx, h.cancel = nil, nil
	h.pcm = h.pcm[:0]
	return nil
}

// answer reports what AgentHub made of the utterance the way Home Assistant's pipeline events are
// reported: the reply's words, then its address, then the end of the run.
func (h *hub) answer(ctx context.Context, pcm []byte) {
	samples := make([]int16, len(pcm)/2)
	for i := range samples {
		samples[i] = int16(uint16(pcm[2*i]) | uint16(pcm[2*i+1])<<8)
	}
	reply, url, err := HubTurn(ctx, samples)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		h.post(event{kind: evError, code: "intent-failed", msg: err.Error()})
		return
	}
	if reply != "" {
		h.post(event{kind: evReplyText, text: reply})
	}
	if url != "" {
		h.post(event{kind: evReplyURL, url: url})
	}
	h.post(event{kind: evRunEnd})
}
