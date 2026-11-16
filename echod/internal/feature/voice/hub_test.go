package voice

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
)

// A turn answered through AgentHub reports the reply's words, then its address, then the end of the run,
// as Home Assistant's pipeline would; one AgentHub kept nothing of ends the run and says nothing.
func TestHubTurn(t *testing.T) {
	config.Use(filepath.Join(t.TempDir(), "state.json"))
	_ = config.Set().AgentHub().URL("http://hub:8787")
	_ = config.Set().AgentHub().Key("k")
	b := config.Get().Brain
	b.Mode = config.BrainAgentHub
	_ = config.Set().Brain().Set(b)
	var got []int16
	reply := "It's three o'clock."
	SetHubTurn(func(_ context.Context, pcm []int16) (string, string, error) {
		got = pcm
		if reply == "" {
			return "", "", nil
		}
		return reply, "http://dot/agenthub/reply?t=x", nil
	})
	t.Cleanup(func() { SetHubTurn(nil) })
	if !hubReady() {
		t.Fatal("not ready with AgentHub chosen, an address, a key and a turn installed")
	}
	events := make(chan event, 8)
	h := newHub(func(e event) { events <- e })
	_ = h.Start("")
	_ = h.Audio([]byte{1, 0, 0xff, 0xff})
	_ = h.End()
	var kinds []eventKind
	for len(kinds) < 3 {
		select {
		case e := <-events:
			kinds = append(kinds, e.kind)
		case <-time.After(2 * time.Second):
			t.Fatalf("events %v", kinds)
		}
	}
	if kinds[0] != evReplyText || kinds[1] != evReplyURL || kinds[2] != evRunEnd || len(got) != 2 || got[1] != -1 {
		t.Fatalf("events %v, samples %v", kinds, got)
	}

	reply = ""
	_ = h.Start("")
	_ = h.End()
	select {
	case e := <-events:
		if e.kind != evRunEnd {
			t.Fatalf("kept nothing, posted %v", e.kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no run end")
	}
}
