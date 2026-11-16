package detect

import (
	"path/filepath"
	"testing"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/lib/endpoint"
	"github.com/HuskerMinion/techo5/echod/internal/lib/safe"
	"github.com/HuskerMinion/techo5/echod/internal/lib/wake"
)

// sound is audio made of 100 ms windows at the given levels.
func sound(levels ...int16) []int16 {
	var out []int16
	for _, l := range levels {
		for i := 0; i < endpoint.Window; i++ {
			if i%2 == 0 {
				out = append(out, l)
			} else {
				out = append(out, -l)
			}
		}
	}
	return out
}

func TestAWordSaidOnItsOwn(t *testing.T) {
	for _, c := range []struct {
		name  string
		audio []int16
		alone bool
	}{
		{"quiet room", sound(50, 50, 50, 50, 3000, 3200, 2800, 60, 50, 50, 40, 50), true},
		{"talking into it", sound(50, 1500, 1800, 1600, 3000, 3200, 2800, 60, 50, 50, 40, 50), false},
		{"talking on after it", sound(50, 50, 50, 50, 3000, 3200, 2800, 1500, 1700, 1600, 1500, 50), false},
		{"a television as loud as the word", sound(1200, 1100, 1300, 1200, 3000, 3200, 2800, 1200, 1300, 1100, 1200, 1300), false},
		{"a quiet television", sound(300, 250, 320, 280, 3000, 3200, 2800, 300, 250, 310, 290, 300), true},
		{"silence", sound(0, 0, 0, 0, 0), false},
	} {
		if got, why := spokenAlone(c.audio); got != c.alone {
			t.Errorf("%s: alone %v (%s), want %v", c.name, got, why, c.alone)
		}
	}
}

// Set to act only when said on its own, a word of your own fires only when the audio around it says it
// stood alone; off, it never listens on.
func TestAWordOfYourOwnCanWaitToBeAlone(t *testing.T) {
	goAlone = func(_ string, f func()) { f() }
	t.Cleanup(func() { goAlone = safe.Go })
	config.Use(filepath.Join(t.TempDir(), "state.json"))
	for _, c := range []struct {
		name  string
		set   bool
		audio []int16
		fired int
		heard int
	}{
		{"off", false, nil, 1, 0},
		{"on, said alone", true, sound(50, 50, 50, 50, 3000, 3200, 60, 50, 50), 1, 1},
		{"on, inside a sentence", true, sound(50, 1500, 1800, 1600, 3000, 3200, 1600, 1700, 1500), 0, 1},
	} {
		r := newCommandRig(t).as(ownCommand(1))
		r.models = append(r.models, model("lights", wake.KindMicroWakeWord))
		r.choose(t, "lights")
		if err := config.Set().OwnCommand(1).Alone(c.set); err != nil {
			t.Fatal(err)
		}
		heard := 0
		r.c.hear = func() []int16 { heard++; return c.audio }
		r.c.heard()
		if len(r.fired) != c.fired || heard != c.heard {
			t.Errorf("%s: fired %d, listened on %d times; want %d and %d", c.name, len(r.fired), heard, c.fired, c.heard)
		}
	}
}
