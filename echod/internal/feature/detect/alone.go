package detect

import (
	"math"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/lib/endpoint"
	"github.com/HuskerMinion/techo5/echod/internal/lib/safe"
)

// goAlone runs the check on a goroutine of its own; a test runs it in place.
var goAlone = safe.Go

// A command word of your own can be set to act only when it was said on its own: "lights" said to the
// room, not "lights" inside a sentence or from a television. Whether it stood alone is told the way
// package endpoint tells the person talking from the room, by level against the speaker's own: the word
// is the loudest thing in the audio around it, and on its own it has quiet just before it and after it.
// Speech running into it or on after it, or a room as loud as the word, keeps the level up. Nothing
// here knows what the word is, so it works for any model in any language.

const (
	// aloneBefore is the audio before acting that is looked at: the word, and what led up to it. The
	// word ends a little before it is acted on (the vote and the nearest device's wait), so it is in here.
	aloneBefore = 2 * time.Second

	// aloneAfter is how long the device listens on after acting for the word, for speech going on.
	aloneAfter = 500 * time.Millisecond

	// aloneLead is how much just before the word has to be quiet: about a breath.
	aloneLead = 3

	// aloneLoud is how many windows after the word may still be at or above quiet: one lets the tail of
	// the word itself or a click through.
	aloneLoud = 1
)

// spokenAlone reports whether the loudest stretch of audio, the word, had quiet just before it and after
// it, quiet being under endpoint.Default.Off of the word's level, and why not when it hadn't.
func spokenAlone(audio []int16) (bool, string) {
	var levels []float64
	for i := 0; i+endpoint.Window <= len(audio); i += endpoint.Window {
		var sum float64
		for _, v := range audio[i : i+endpoint.Window] {
			sum += float64(v) * float64(v)
		}
		levels = append(levels, math.Sqrt(sum/endpoint.Window))
	}
	peakAt := 0
	for i, l := range levels {
		if l > levels[peakAt] {
			peakAt = i
		}
	}
	if len(levels) == 0 || levels[peakAt] == 0 {
		return false, "nothing heard"
	}
	on, off := endpoint.Default.On*levels[peakAt], endpoint.Default.Off*levels[peakAt]

	// The word is the run of windows at speaking level around the loudest one.
	start, end := peakAt, peakAt
	for start > 0 && levels[start-1] >= on {
		start--
	}
	for end < len(levels)-1 && levels[end+1] >= on {
		end++
	}

	if start < aloneLead {
		return false, "the word is too near the start to hear what led up to it"
	}
	for _, l := range levels[start-aloneLead : start] {
		if l >= off {
			return false, "speech or noise ran into it"
		}
	}
	loud := 0
	for _, l := range levels[end+1:] {
		if l >= off {
			loud++
		}
	}
	if loud > aloneLoud {
		return false, "speech or noise went on after it"
	}
	return true, ""
}
