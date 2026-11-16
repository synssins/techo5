package agenthub

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HuskerMinion/techo5/echod/internal/config"
	"github.com/HuskerMinion/techo5/echod/internal/feature/media"
	"github.com/HuskerMinion/techo5/echod/internal/feature/voice"
	"github.com/HuskerMinion/techo5/echod/internal/feature/web"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/mic"
	"github.com/HuskerMinion/techo5/echod/internal/hardware/speaker"
	"github.com/HuskerMinion/techo5/echod/internal/lib/endpoint"
	"github.com/HuskerMinion/techo5/echod/internal/lib/hass"
	"github.com/HuskerMinion/techo5/echod/internal/lib/safe"
)

// Listening without a wake word, against AgentHub's voice-device protocol (its /voice-devices.txt):
// every stretch of speech goes up as a WAV (/api/device/listen); one AgentHub accepts as said to an agent
// is followed by its reply (/api/device/reply), whose audio is played here or on the player replies play
// on, and /api/device/played opens AgentHub's window for a follow-up without the agent's name. Nothing is
// sent while the device makes a sound of its own or a turn has the microphones.

const (
	// preRoll is the audio kept from before speech was noticed: the first syllable starts quieter than
	// what tells it apart from the room.
	preRoll = 500 * time.Millisecond
	// onsetDB is how far over the room's floor a window has to be to count as speech starting, and
	// onsetWindows how many in a row: one loud window is a door or a cough.
	onsetDB      = 12.0
	onsetWindows = 2
	// longest is AgentHub's own limit on one utterance.
	longest = 30 * time.Second
	// afterSound is how long the room is left alone after the device's own sound ends: its tail is still
	// in the room, and AgentHub would only throw it away as echo.
	afterSound = 700 * time.Millisecond
	// replyFor is how long a reply is waited for before giving up on it.
	replyFor = 2 * time.Minute
	// servedFor is how long a reply's audio stays at its address for a player elsewhere to fetch.
	servedFor = 3 * time.Minute
	// fullScale is the int16 full scale the microphone frames are narrowed to (mic.Floor's).
	fullScale = 32768.0
	// servePath is where the device serves a reply for the player replies play on.
	servePath = "/agenthub/reply"
)

// errRefused is a 401: the key is wrong, and sending more would only be refused again.
var errRefused = errors.New("the device key was refused")

type loop struct {
	report func(string)
	kick   chan struct{}
	client *http.Client

	// soundEnded is when this feature's own playback last ended (Unix nanoseconds), and playing whether
	// one is going on.
	soundEnded atomic.Int64
	playing    atomic.Bool

	mu     sync.Mutex
	stop   func() // stops the reply playing now, nil when none is
	served map[string]reply
}

type reply struct {
	body  []byte
	ctype string
	until time.Time
}

func newLoop(report func(string)) *loop {
	l := &loop{report: report, kick: make(chan struct{}, 1), client: &http.Client{Timeout: 40 * time.Second},
		served: map[string]reply{}}
	web.Handle(servePath, "", l.serving, l.serve)
	return l
}

// changed has the loop look at the settings again.
func (l *loop) changed() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// Run listens while the settings say to, starting over whenever they change.
func (f *Feature) Run(ctx context.Context) error { return f.loop.run(ctx) }

func (l *loop) run(ctx context.Context) error {
	for {
		a := config.Get().AgentHub
		switch {
		case !a.On:
			l.report("off")
		case a.URL == "":
			l.report("needs the AgentHub address")
		case a.Key == "":
			l.report("needs the device key")
		default:
			sctx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			safe.Go("agenthub listen", func() { done <- l.session(sctx, a) })
			select {
			case <-ctx.Done():
				cancel()
				<-done
				return nil
			case <-l.kick:
				cancel()
				<-done
				continue
			case err := <-done:
				cancel()
				if errors.Is(err, errRefused) {
					slog.Error("agenthub: the device key was refused (401); not sending until the settings change")
					l.report("key refused")
					break
				}
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-l.kick:
		}
	}
}

// session listens until ctx ends or the key is refused.
func (l *loop) session(ctx context.Context, a config.AgentHub) error {
	frames, _, unlisten := mic.Get().ListenHistory("agenthub", 0)
	defer unlisten()
	l.report("listening")
	slog.Info("agenthub: listening without a wake word", "server", a.URL)

	var (
		window  []int16 // the window being filled
		pre     []int16 // the last preRoll of audio, while nothing is being collected
		seg     []int16 // the speech being collected, nil when none is
		loud    int     // windows in a row over the onset level
		det     *endpoint.Detector
		keepPre = int(preRoll.Seconds() * mic.Rate)
		most    = int(longest.Seconds() * mic.Rate)
	)
	for {
		var frame []int16
		select {
		case <-ctx.Done():
			return nil
		case frame = <-frames:
		}
		if l.quietPlease() {
			window, pre, seg, loud, det = window[:0], pre[:0], nil, 0, nil
			continue
		}
		if seg != nil {
			seg = append(seg, frame...)
			if det.Feed(frame) || len(seg) >= most {
				// The detector was fed from the start of seg, pre-roll included.
				end := len(seg)
				if at := det.EndedAt(); at > 0 && at+endpoint.Window*2 < end {
					end = at + endpoint.Window*2 // a little after the last word
				}
				utterance := seg[:min(end, len(seg))]
				seg, det, loud = nil, nil, 0
				if err := l.send(ctx, a, utterance); errors.Is(err, errRefused) {
					return err
				}
			}
			continue
		}
		pre = append(pre, frame...)
		if len(pre) > keepPre {
			pre = pre[len(pre)-keepPre:]
		}
		window = append(window, frame...)
		for len(window) >= endpoint.Window {
			if dbfs(window[:endpoint.Window]) > mic.Get().Floor()+onsetDB {
				loud++
			} else {
				loud = 0
			}
			window = window[endpoint.Window:]
			if loud >= onsetWindows {
				seg = append([]int16(nil), pre...)
				det = endpoint.New(endpoint.Default)
				det.Feed(seg)
				pre, window = pre[:0], window[:0]
				break
			}
		}
	}
}

// quietPlease is whether the room is the device's or a turn's just now: its own sound and the moment
// after it, a turn, or anything else holding the speaker.
func (l *loop) quietPlease() bool {
	if l.playing.Load() || voice.Get().Busy() || speaker.Sound().Busy() {
		return true
	}
	return time.Since(time.Unix(0, l.soundEnded.Load())) < afterSound
}

func dbfs(w []int16) float64 {
	var sum float64
	for _, v := range w {
		sum += float64(v) * float64(v)
	}
	rms := math.Sqrt(sum / float64(len(w)))
	if rms == 0 {
		return -120
	}
	return 20 * math.Log10(rms/fullScale)
}

type listenAnswer struct {
	Accepted  bool   `json:"accepted"`
	To        string `json:"to"`
	Reason    string `json:"reason"`
	MessageID int64  `json:"message_id"`
}

// send posts one utterance and, when it was said to an agent, waits for the reply on a goroutine of its
// own, so the room is heard meanwhile.
func (l *loop) send(ctx context.Context, a config.AgentHub, pcm []int16) error {
	var ans listenAnswer
	status, err := l.call(ctx, a, http.MethodPost, "/api/device/listen", "audio/wav", wav(pcm), &ans)
	switch {
	case status == http.StatusUnauthorized:
		return errRefused
	case err != nil:
		slog.Warn("agenthub: sending what was heard failed", "err", err)
		l.report("server unreachable")
		// A short wait, so a server that is down is not asked again at every word.
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
		return nil
	}
	l.report("listening")
	if ans.Reason == "stopped" {
		l.stopReply()
		return nil
	}
	if !ans.Accepted {
		return nil
	}
	slog.Info("agenthub: said to an agent", "to", ans.To, "reason", ans.Reason, "message", ans.MessageID)
	safe.Go("agenthub reply", func() { l.answer(ctx, a, ans.MessageID) })
	return nil
}

type replyAnswer struct {
	Text      *string `json:"text"`
	AudioURL  string  `json:"audio_url"`
	MessageID int64   `json:"message_id"`
}

// answer waits for the agent's reply to message after, plays it, and says so.
func (l *loop) answer(ctx context.Context, a config.AgentHub, after int64) {
	deadline := time.Now().Add(replyFor)
	var r replyAnswer
	for {
		if ctx.Err() != nil || time.Now().After(deadline) {
			slog.Warn("agenthub: no reply came", "after", after)
			return
		}
		r = replyAnswer{}
		q := "/api/device/reply?after=" + strconv.FormatInt(after, 10) + "&wait=25"
		status, err := l.call(ctx, a, http.MethodGet, q, "", nil, &r)
		if status == http.StatusUnauthorized {
			return
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if r.Text != nil {
			break
		}
	}
	if r.AudioURL == "" {
		slog.Warn("agenthub: the reply has no audio", "message", r.MessageID)
		return
	}
	body, ctype, err := l.fetch(ctx, a, r.AudioURL)
	if err != nil {
		slog.Warn("agenthub: fetching the reply's audio failed", "err", err)
		return
	}
	if l.play(ctx, body, ctype) {
		if _, err := l.call(ctx, a, http.MethodPost, "/api/device/played", "", nil, nil); err != nil {
			slog.Warn("agenthub: saying the reply was played failed", "err", err)
		}
	}
}

// play plays a reply here, or on the player replies play on (config.Brain.ReplyPlayer), and reports
// whether it was heard to the end.
func (l *loop) play(ctx context.Context, body []byte, ctype string) bool {
	samples, err := media.Decode(body, ctype, "agenthub reply")
	if err != nil {
		slog.Warn("agenthub: the reply's audio could not be read", "err", err)
		return false
	}
	length := time.Duration(len(samples)) * time.Second / speaker.VoiceRate
	l.playing.Store(true)
	defer func() {
		l.soundEnded.Store(time.Now().UnixNano())
		l.playing.Store(false)
	}()

	if player := config.Get().Brain.ReplyPlayer; player != "" {
		if u, ok := l.offer(body, ctype); ok {
			if err := hass.Get().Call("media_player", "play_media", map[string]any{
				"entity_id": player, "media_content_id": u, "media_content_type": "music", "announce": true,
			}); err == nil {
				pctx, cancel := context.WithCancel(ctx)
				l.setStop(func() {
					cancel()
					_ = hass.Get().Call("media_player", "media_stop", map[string]any{"entity_id": player})
				})
				defer l.setStop(nil)
				// The device cannot hear another speaker finish: the reply's length and a moment for it to
				// start, as the voice feature's own replies elsewhere are waited for.
				t := time.NewTimer(length + 2*time.Second)
				defer t.Stop()
				select {
				case <-t.C:
					return true
				case <-pctx.Done():
					return false
				}
			} else {
				slog.Warn("agenthub: the player did not take the reply, playing it here", "player", player, "err", err)
			}
		}
	}

	media.Get().Sounding(true)
	defer media.Get().Sounding(false)
	claim := speaker.Sound().ClaimSpeech("agenthub", func(_ context.Context, p *speaker.Player) error {
		p.PlayVoice(samples)
		return nil
	})
	l.setStop(func() {
		if speaker.Sound().Holder() == "agenthub" {
			speaker.Sound().Silence()
		}
	})
	defer l.setStop(nil)
	select {
	case <-claim.Done():
		return !claim.Stopped() && claim.Err() == nil
	case <-ctx.Done():
		return false
	}
}

func (l *loop) setStop(f func()) {
	l.mu.Lock()
	l.stop = f
	l.mu.Unlock()
}

// stopReply is AgentHub hearing "stop" said to the agent: the reply playing ends now.
func (l *loop) stopReply() {
	l.mu.Lock()
	f := l.stop
	l.mu.Unlock()
	if f != nil {
		slog.Info("agenthub: stopped")
		f()
	}
}

// offer serves a reply at an address of this device's own for the player elsewhere to fetch, which
// cannot send AgentHub's key: a random one, gone after servedFor.
func (l *loop) offer(body []byte, ctype string) (string, bool) {
	ip := ownIP()
	if ip == "" {
		return "", false
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", false
	}
	tok := hex.EncodeToString(b)
	l.mu.Lock()
	for k, r := range l.served {
		if time.Now().After(r.until) {
			delete(l.served, k)
		}
	}
	l.served[tok] = reply{body: body, ctype: ctype, until: time.Now().Add(servedFor)}
	l.mu.Unlock()
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(web.Port)) + servePath + "?t=" + tok, true
}

func (l *loop) serving() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.served) > 0
}

func (l *loop) serve(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	rep, ok := l.served[r.URL.Query().Get("t")]
	l.mu.Unlock()
	if !ok || time.Now().After(rep.until) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", rep.ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(rep.body)))
	_, _ = w.Write(rep.body)
}

// ownIP is the device's address on its Wi-Fi.
func ownIP() string {
	ifc, err := net.InterfaceByName("wlan0")
	if err != nil {
		return ""
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
}

// call makes one request to AgentHub with the device's key, decoding a JSON answer into out when out is
// not nil. A 5xx is an error; the status is returned either way.
func (l *loop) call(ctx context.Context, a config.AgentHub, method, path, ctype string, body []byte, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.URL+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Key)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// fetch gets a reply's audio, which needs the key as every call does.
func (l *loop) fetch(ctx context.Context, a config.AgentHub, path string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL+path, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.Key)
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%s: %s", path, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return body, resp.Header.Get("Content-Type"), err
}

// wav is mono 16-bit PCM at the microphone's rate in a WAV file.
func wav(pcm []int16) []byte {
	var b bytes.Buffer
	n := len(pcm) * 2
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+n))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(mic.Rate), uint32(mic.Rate * 2), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(n))
	_ = binary.Write(&b, binary.LittleEndian, pcm)
	return b.Bytes()
}
