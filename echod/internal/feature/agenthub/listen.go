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
	// longest is the most one utterance sends; AgentHub takes up to 30 s and 2 MB.
	longest = 20 * time.Second
	// afterSound is how long the room is left alone after the device's own sound ends: its tail is still
	// in the room, and AgentHub would only throw it away as echo.
	afterSound = 700 * time.Millisecond
	// replyFor is how long a reply is waited for before giving up on it.
	replyFor = 2 * time.Minute
	// servedFor is how long a reply's address answers, for the player to start fetching it.
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

	// refused is the key turned away by a send running on its own: the session ends on it.
	refused atomic.Bool

	mu     sync.Mutex
	stop   func() // stops the reply playing now, nil when none is
	served map[string]*relay
}

// relay is a reply passed on from AgentHub as it arrives, at an address of this device's own: the player
// (this device's own, or one elsewhere) cannot send AgentHub's key.
type relay struct {
	a     config.AgentHub
	path  string
	until time.Time

	// first is when its audio began going out, and rate and bytes what went: the length of what was
	// played, for a player elsewhere that cannot be heard finishing. done closes once it is all sent.
	mu    sync.Mutex
	first time.Time
	rate  int
	bytes int
	done  chan struct{}
}

func newLoop(report func(string)) *loop {
	l := &loop{report: report, kick: make(chan struct{}, 1), client: &http.Client{Timeout: 40 * time.Second},
		served: map[string]*relay{}}
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
			safe.Go("agenthub listen", func() { done <- l.session(sctx, cancel, a) })
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

// session listens until ctx ends or the key is refused (cancel, from a send).
func (l *loop) session(ctx context.Context, cancel context.CancelFunc, a config.AgentHub) error {
	l.refused.Store(false)
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
			if l.refused.Load() {
				return errRefused
			}
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
				// On a goroutine of its own, so the room goes on being heard while it uploads.
				safe.Go("agenthub send", func() {
					if errors.Is(l.send(ctx, a, utterance), errRefused) {
						l.refused.Store(true)
						cancel()
					}
				})
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
				// The pause set on the device (End of speech pause), as a turn's: a window is a tenth of a second.
				ends := endpoint.Default
				ends.Quiet = config.Get().Microphone.EndPauseTenths()
				det = endpoint.New(ends)
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
		slog.Warn("agenthub: sending what was heard failed", "err", err,
			"seconds", float64(len(pcm))/mic.Rate, "bytes", 44+2*len(pcm))
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
	r, ok := l.waitReply(ctx, a, after)
	if !ok {
		return
	}
	if r.AudioURL == "" {
		slog.Warn("agenthub: the reply has no audio", "message", r.MessageID)
		return
	}
	if l.play(ctx, a, r.AudioURL) {
		if _, err := l.call(ctx, a, http.MethodPost, "/api/device/played", "", nil, nil); err != nil {
			slog.Warn("agenthub: saying the reply was played failed", "err", err)
		}
	}
}

// waitReply asks for the reply to message after until one comes or replyFor has passed.
func (l *loop) waitReply(ctx context.Context, a config.AgentHub, after int64) (replyAnswer, bool) {
	deadline := time.Now().Add(replyFor)
	for {
		if ctx.Err() != nil || time.Now().After(deadline) {
			slog.Warn("agenthub: no reply came", "after", after)
			return replyAnswer{}, false
		}
		var r replyAnswer
		q := "/api/device/reply?after=" + strconv.FormatInt(after, 10) + "&wait=25"
		status, err := l.call(ctx, a, http.MethodGet, q, "", nil, &r)
		if status == http.StatusUnauthorized {
			return replyAnswer{}, false
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return replyAnswer{}, false
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if r.Text != nil {
			return r, true
		}
	}
}

// turn answers a turn the device opened itself (its wake word or a command word), for the voice
// feature's AgentHub backend: sent as direct, so AgentHub skips its name check and its agent answers
// without thinking. The reply's address is this device's relay at its own address, which a player
// elsewhere can fetch too.
func (l *loop) turn(ctx context.Context, pcm []int16) (string, string, error) {
	a := config.Get().AgentHub
	var ans listenAnswer
	status, err := l.call(ctx, a, http.MethodPost, "/api/device/listen?direct=1", "audio/wav", wav(pcm), &ans)
	switch {
	case status == http.StatusUnauthorized:
		l.report("key refused")
		return "", "", errRefused
	case err != nil:
		return "", "", err
	case !ans.Accepted:
		return "", "", nil
	}
	r, ok := l.waitReply(ctx, a, ans.MessageID)
	if !ok {
		return "", "", errors.New("no reply came")
	}
	if r.AudioURL == "" {
		return *r.Text, "", nil
	}
	host := ownIP()
	if host == "" {
		host = "127.0.0.1"
	}
	u, _, _ := l.offer(a, r.AudioURL, host)
	return *r.Text, u, nil
}

// play plays a reply as it arrives, here or on the player replies play on (config.Brain.ReplyPlayer),
// and reports whether it was heard to the end.
func (l *loop) play(ctx context.Context, a config.AgentHub, path string) bool {
	l.playing.Store(true)
	defer func() {
		l.soundEnded.Store(time.Now().UnixNano())
		l.playing.Store(false)
	}()

	if player := config.Get().Brain.ReplyPlayer; player != "" {
		if u, r, ok := l.offer(a, path, ownIP()); ok {
			err := hass.Get().Call("media_player", "play_media", map[string]any{
				"entity_id": player, "media_content_id": u, "media_content_type": "music", "announce": true,
			})
			if err == nil {
				pctx, cancel := context.WithCancel(ctx)
				defer cancel()
				l.setStop(func() {
					cancel()
					_ = hass.Get().Call("media_player", "media_stop", map[string]any{"entity_id": player})
				})
				defer l.setStop(nil)
				return r.heardElsewhere(pctx)
			}
			slog.Warn("agenthub: the player did not take the reply, playing it here", "player", player, "err", err)
		}
	}

	u, _, ok := l.offer(a, path, "127.0.0.1")
	if !ok {
		return false
	}
	media.Get().Sounding(true)
	defer media.Get().Sounding(false)
	claim := speaker.Sound().ClaimSpeech("agenthub", func(cctx context.Context, p *speaker.Player) error {
		return media.PlaySpeech(cctx, u, &media.SpeechStream{Buffer: media.AnnounceBuffer, Gain: true}, p)
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

// heardElsewhere waits until a reply on a player elsewhere should have been heard: the device cannot
// hear another speaker finish, so it is the length of what went out, from when it began, and a moment
// for the player to start, as the voice feature's own replies elsewhere are waited for.
func (r *relay) heardElsewhere(ctx context.Context) bool {
	select {
	case <-r.done:
	case <-ctx.Done():
		return false
	case <-time.After(servedFor):
		return false
	}
	r.mu.Lock()
	first, rate, n := r.first, r.rate, r.bytes
	r.mu.Unlock()
	if rate == 0 {
		rate = speaker.VoiceRate
	}
	length := time.Duration(n) * time.Second / time.Duration(rate*2)
	t := time.NewTimer(time.Until(first.Add(length + 2*time.Second)))
	defer t.Stop()
	select {
	case <-t.C:
		return true
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

// offer gives a reply an address on this device, at host, that relays it from AgentHub with the key: a
// random one, answering once, within servedFor.
func (l *loop) offer(a config.AgentHub, path, host string) (string, *relay, bool) {
	if host == "" {
		return "", nil, false
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", nil, false
	}
	tok := hex.EncodeToString(b)
	r := &relay{a: a, path: path, until: time.Now().Add(servedFor), done: make(chan struct{})}
	l.mu.Lock()
	for k, old := range l.served {
		if time.Now().After(old.until) {
			delete(l.served, k)
		}
	}
	l.served[tok] = r
	l.mu.Unlock()
	return "http://" + net.JoinHostPort(host, strconv.Itoa(web.Port)) + servePath + "?t=" + tok, r, true
}

func (l *loop) serving() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.served) > 0
}

// serve relays a reply from AgentHub as it arrives. The address is used up by the fetch.
func (l *loop) serve(w http.ResponseWriter, req *http.Request) {
	tok := req.URL.Query().Get("t")
	l.mu.Lock()
	r, ok := l.served[tok]
	delete(l.served, tok)
	l.mu.Unlock()
	if !ok || time.Now().After(r.until) {
		http.NotFound(w, req)
		return
	}
	defer close(r.done)
	up, err := http.NewRequestWithContext(req.Context(), http.MethodGet, r.a.URL+r.path, nil)
	if err != nil {
		http.Error(w, "bad reply address", http.StatusBadGateway)
		return
	}
	up.Header.Set("Authorization", "Bearer "+r.a.Key)
	resp, err := (&http.Client{}).Do(up) // no timeout: a reply is spoken for as long as it takes
	if err != nil {
		slog.Warn("agenthub: fetching the reply's audio failed", "err", err)
		http.Error(w, "reply unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("agenthub: fetching the reply's audio failed", "status", resp.Status)
		http.Error(w, "reply unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	flush, _ := w.(http.Flusher)
	buf := make([]byte, 8192)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			r.sent(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flush != nil {
				flush.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// sent counts what went out, reading the sample rate from a WAV header at the front.
func (r *relay) sent(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.first.IsZero() {
		r.first = time.Now()
		if len(p) >= 28 && string(p[0:4]) == "RIFF" {
			r.rate = int(binary.LittleEndian.Uint32(p[24:28]))
			r.bytes -= 44
		}
	}
	r.bytes += len(p)
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
