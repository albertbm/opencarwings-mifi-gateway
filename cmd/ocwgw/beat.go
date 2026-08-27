// Heartbeat. opencarwings cannot tell you the gateway has gone away: a wake-up is
// handed to a channel group and the send returns true whether or not anyone is
// listening. So the gateway says so itself, pushing a status line to a URL you
// set and letting whatever is on the other end shout when the pushes stop.
//
// None of this may stall the websocket loop or the AT channel. See
// docs/heartbeat.md.
package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBeatEvery = 5 * time.Minute
	minBeatEvery     = time.Minute      // floor, this runs on a metered SIM
	beatTimeout      = 10 * time.Second // hard cap on a single push
	beatCheckEvery   = 10 * time.Second // how often state is examined, not how often we push
	beatDebounce     = 60 * time.Second // a fault holds this long before it is called down
	beatStartGrace   = 2 * time.Minute  // at startup, this long to come up before it is called down
	beatMinGap       = 60 * time.Second // floor between change-triggered pushes, so a flap cannot spam
	beatMsgMax       = 200              // fits a URL, an nginx request line and a phone notification
)

// beatWake nudges the loop when the heartbeat config changes.
var beatWake = make(chan struct{}, 1)

func signalBeatWake() {
	select {
	case beatWake <- struct{}{}:
	default:
	}
}

// One client for the life of the process. A fresh TLS handshake is ~3.5 KB against
// ~600 bytes for the beat, so the idle timeout outlives any sane interval.
var (
	beatClientOnce sync.Once
	beatClientVal  *http.Client
)

func beatHTTP() *http.Client {
	beatClientOnce.Do(func() {
		beatClientVal = &http.Client{
			Timeout: beatTimeout,
			Transport: &http.Transport{
				TLSClientConfig:     &tls.Config{RootCAs: rootPool()},
				MaxIdleConns:        2,
				MaxIdleConnsPerHost: 1,
				IdleConnTimeout:     30 * time.Minute,
				DisableCompression:  true,
			},
		}
	})
	return beatClientVal
}

func parseBeatEvery(s string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d <= 0 {
		return defaultBeatEvery
	}
	if d < minBeatEvery {
		return minBeatEvery
	}
	return d
}

// deviceUptime is how long the modem has been powered, not how long this process
// has run. The two diverging is what separates a gateway restart from a reboot.
func deviceUptime() time.Duration {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

func fmtDur(d time.Duration) string {
	if d <= 0 {
		return "?"
	}
	days := int(d / (24 * time.Hour))
	hours := int(d/time.Hour) % 24
	mins := int(d/time.Minute) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%02dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%02dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

func fmtAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	if d := time.Since(t); d < time.Minute {
		return "just now"
	}
	return fmtDur(time.Since(t)) + " ago"
}

// clip counts runes, not bytes, so a cut never lands inside a character.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// beatMessage is written to be read in a phone notification, not parsed. When the
// gateway is down the fault leads.
func beatMessage(v beatView, up, showErr bool) string {
	life := "up " + fmtDur(v.uptime)
	if dev := deviceUptime(); dev > 0 {
		life += " dev " + fmtDur(dev)
	}
	at := "down"
	if v.atOK {
		at = "ok"
	}
	link := fmt.Sprintf("ws=%s · at=%s", v.wsState, at)
	counts := fmt.Sprintf("sent %d/%d", v.sentOK, v.sentErr)

	parts := []string{life, link, counts}
	if !up {
		parts = []string{link, life, counts}
	}
	if v.lastSent != "" {
		parts = append(parts, fmt.Sprintf("last %q %s", v.lastSent, fmtAge(v.lastSentAt)))
	}
	if showErr && v.lastEvent != "" {
		// last_event is as often something ordinary ("server acknowledged
		// connection"), so calling it an error unless we are down would make
		// every message read like a fault.
		label := "note"
		if !up {
			label = "err"
		}
		parts = append(parts, fmt.Sprintf("%s %q", label, v.lastEvent))
	}
	return clip(strings.Join(parts, " · "), beatMsgMax)
}

// pushBeat sends one beat. status/msg/ping are what an Uptime Kuma push monitor
// reads, and anything else ignores them. No body: Kuma discards it, and relying
// on one would tie the format to a single receiver.
func pushBeat(raw string, up bool, msg string, ping int) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	status := "down"
	if up {
		status = "up"
	}
	q := u.Query()
	q.Set("status", status)
	q.Set("msg", msg)
	q.Set("ping", strconv.Itoa(ping))
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "ocwgw")
	resp, err := beatHTTP().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain so the connection goes back in the idle pool instead of being dropped.
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http %s", resp.Status)
	}
	return nil
}

// beatState is the decision the loop carries between checks. It is separate from
// the loop so the rules can be tested without a clock or a network.
type beatState struct {
	reported     bool
	haveReported bool
	lastBeatAt   time.Time
	lastChangeAt time.Time
	downSince    time.Time
}

// decide answers whether to push now, and what status to push.
//
// Startup is the case worth explaining. A gateway that has just booted is down
// for a few seconds by definition, and pushing that would mean a down alert on
// every restart, so nothing goes out until it comes up or beatStartGrace passes.
func (b *beatState) decide(up bool, now time.Time, every time.Duration) (send, want bool) {
	if up {
		b.downSince = time.Time{}
	} else if b.downSince.IsZero() {
		b.downSince = now
	}

	settle := beatDebounce
	if !b.haveReported {
		settle = beatStartGrace
	}

	hold := false
	switch {
	case up:
		want = true
	case now.Sub(b.downSince) >= settle:
		want = false
	case b.haveReported:
		want = b.reported // fault not confirmed yet, keep saying what we last said
	default:
		hold = true // nothing said yet and it may still be coming up
	}

	switch {
	case hold:
	case !b.haveReported:
		send = true
	case want != b.reported && now.Sub(b.lastChangeAt) >= beatMinGap:
		send = true
	case now.Sub(b.lastBeatAt) >= every:
		send = true
	}
	return send, want
}

// sent records a push that actually went through. A push that failed leaves the
// remembered state alone, so the change is tried again rather than lost.
func (b *beatState) sent(want bool, now time.Time) {
	if want != b.reported {
		b.lastChangeAt = now
	}
	b.reported, b.haveReported = want, true
}

// beatLoop examines the status every beatCheckEvery and pushes when decide says
// so. It reads a snapshot and nothing else, so it keeps reporting when the modem
// and the websocket are broken.
func beatLoop() {
	var (
		b         beatState
		prevEvent string
	)

	for {
		beatURL, every := cfg.getBeat()
		if beatURL == "" {
			select {
			case <-beatWake:
			case <-time.After(30 * time.Second):
			}
			continue
		}

		v := st.view()
		now := time.Now()
		// A live socket with a wedged modem cannot send an SMS, so it is not up.
		up := v.wsState == "connected" && v.atOK

		if send, want := b.decide(up, now, every); send {
			msg := beatMessage(v, want, !want || v.lastEvent != prevEvent)
			b.lastBeatAt = now
			if err := pushBeat(beatURL, want, msg, v.reconn1h); err != nil {
				log.Printf("[beat] push failed: %v", err)
				st.beat(false, err.Error())
			} else {
				b.sent(want, now)
				prevEvent = v.lastEvent
				st.beat(true, msg)
			}
		}

		select {
		case <-beatWake:
		case <-time.After(beatCheckEvery):
		}
	}
}

type beatView struct {
	wsState    string
	atOK       bool
	sentOK     int
	sentErr    int
	lastSent   string
	lastSentAt time.Time
	lastEvent  string
	uptime     time.Duration
	reconn1h   int
}

// view takes everything the beat needs under one lock, so no lock is held across
// the HTTP request.
func (s *gwStatus) view() beatView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return beatView{
		wsState:    s.wsState,
		atOK:       s.atOK,
		sentOK:     s.sentOK,
		sentErr:    s.sentErr,
		lastSent:   s.lastSent,
		lastSentAt: s.lastSentAt,
		lastEvent:  s.lastEvent,
		uptime:     time.Since(s.started),
		reconn1h:   s.reconnectsLocked(time.Hour),
	}
}

// reconnectsLocked counts websocket connects within d. Callers hold s.mu.
func (s *gwStatus) reconnectsLocked(d time.Duration) int {
	cut := time.Now().Add(-d)
	n := 0
	for _, t := range s.wsConns {
		if t.After(cut) {
			n++
		}
	}
	return n
}
