package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseBeatEvery(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", defaultBeatEvery},
		{"nonsense", defaultBeatEvery},
		{"0s", defaultBeatEvery},
		{"-5m", defaultBeatEvery},
		{"10s", minBeatEvery}, // clamped, this runs on a metered SIM
		{"5m", 5 * time.Minute},
		{" 15m ", 15 * time.Minute},
	}
	for _, c := range cases {
		if got := parseBeatEvery(c.in); got != c.want {
			t.Errorf("parseBeatEvery(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestFmtDur(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "?"},
		{-time.Hour, "?"},
		{90 * time.Second, "1m"},
		{45 * time.Minute, "45m"},
		{4*time.Hour + 12*time.Minute, "4h12m"},
		{76*time.Hour + 4*time.Minute, "3d04h"},
	}
	for _, c := range cases {
		if got := fmtDur(c.in); got != c.want {
			t.Errorf("fmtDur(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBeatMessageLeadsWithTheFault(t *testing.T) {
	v := beatView{wsState: "disconnected", atOK: true, sentOK: 12, sentErr: 1, uptime: 2 * time.Hour}
	msg := beatMessage(v, false, false)
	if !strings.HasPrefix(msg, "ws=disconnected") {
		t.Errorf("down beat should lead with the fault, got %q", msg)
	}

	v.wsState = "connected"
	msg = beatMessage(v, true, false)
	if !strings.HasPrefix(msg, "up ") {
		t.Errorf("up beat should lead with uptime, got %q", msg)
	}
}

func TestBeatMessageErrOnlyWhenAsked(t *testing.T) {
	v := beatView{wsState: "connected", atOK: true, lastEvent: "unknown server type: nope", uptime: time.Hour}
	if strings.Contains(beatMessage(v, true, false), "note ") {
		t.Error("the event should be left out unless the caller asks for it")
	}
	// Up: an ordinary event is a note, not a fault.
	if !strings.Contains(beatMessage(v, true, true), `note "unknown server type: nope"`) {
		t.Error("an event on a healthy gateway should be labelled note")
	}
	// Down: the same field is the fault, so it is an error.
	if !strings.Contains(beatMessage(v, false, true), `err "unknown server type: nope"`) {
		t.Error("an event on a down gateway should be labelled err")
	}
}

func TestBeatMessageFitsTheLimit(t *testing.T) {
	v := beatView{
		wsState:   "connected",
		atOK:      true,
		lastEvent: strings.Repeat("é", 400), // multibyte on purpose: a cut must not split a rune
		uptime:    time.Hour,
	}
	msg := beatMessage(v, true, true)
	if n := len([]rune(msg)); n > beatMsgMax {
		t.Errorf("message is %d runes, limit is %d", n, beatMsgMax)
	}
	if !strings.HasSuffix(msg, "…") {
		t.Errorf("a clipped message should end in an ellipsis, got %q", msg)
	}
}

func TestPushBeatKeepsTheReceiversOwnURL(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := pushBeat(srv.URL+"/api/push/tok3n?extra=keep", true, "up 1h00m", 3); err != nil {
		t.Fatalf("pushBeat: %v", err)
	}
	if got == nil {
		t.Fatal("receiver saw no request")
	}
	if got.URL.Path != "/api/push/tok3n" {
		t.Errorf("path = %q, want /api/push/tok3n", got.URL.Path)
	}
	q := got.URL.Query()
	for k, want := range map[string]string{"status": "up", "msg": "up 1h00m", "ping": "3", "extra": "keep"} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
}

func TestPushBeatReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	if err := pushBeat(srv.URL, false, "down", 0); err == nil {
		t.Error("a 404 should come back as an error, a wrong URL must not look healthy")
	}
}

func TestReconnectCountWindow(t *testing.T) {
	s := &gwStatus{}
	now := time.Now()
	s.wsConns = []time.Time{now.Add(-3 * time.Hour), now.Add(-30 * time.Minute), now.Add(-time.Minute)}
	if got := s.reconnectsLocked(time.Hour); got != 2 {
		t.Errorf("reconnects in the last hour = %d, want 2", got)
	}
}

func TestWsConnectedKeepsTheRingBounded(t *testing.T) {
	s := &gwStatus{}
	for i := 0; i < 200; i++ {
		s.wsConnected()
	}
	if len(s.wsConns) > 64 {
		t.Errorf("ring grew to %d entries", len(s.wsConns))
	}
}

func TestDecideHoldsAtStartupUntilItComesUp(t *testing.T) {
	var b beatState
	t0 := time.Now()

	// Booting: down, but nothing has been reported yet, so say nothing.
	if send, _ := b.decide(false, t0, 5*time.Minute); send {
		t.Error("pushed a down beat while still starting up")
	}
	if send, _ := b.decide(false, t0.Add(30*time.Second), 5*time.Minute); send {
		t.Error("pushed a down beat inside the startup grace")
	}
	// It comes up: first beat, and it says up.
	send, want := b.decide(true, t0.Add(40*time.Second), 5*time.Minute)
	if !send || !want {
		t.Errorf("first beat after coming up: send=%v want=%v", send, want)
	}
}

func TestDecideReportsDownWhenItNeverComesUp(t *testing.T) {
	var b beatState
	t0 := time.Now()
	b.decide(false, t0, 5*time.Minute)
	send, want := b.decide(false, t0.Add(beatStartGrace+time.Second), 5*time.Minute)
	if !send || want {
		t.Errorf("a gateway that never came up should report down: send=%v want=%v", send, want)
	}
}

func TestDecideDebouncesAFault(t *testing.T) {
	var b beatState
	t0 := time.Now()
	send, _ := b.decide(true, t0, 5*time.Minute)
	b.lastBeatAt = t0
	b.sent(true, t0)
	if !send {
		t.Fatal("expected a first beat")
	}

	// A blip inside the debounce window is not reported.
	if send, want := b.decide(false, t0.Add(10*time.Second), 5*time.Minute); send || !want {
		t.Errorf("a 10s blip should not push a down beat: send=%v want=%v", send, want)
	}
	// Recovering leaves the reported state untouched.
	if send, _ := b.decide(true, t0.Add(20*time.Second), 5*time.Minute); send {
		t.Error("recovering from an unreported blip should not push")
	}
	// A fault that holds does get reported.
	b.decide(false, t0.Add(30*time.Second), 5*time.Minute)
	send, want := b.decide(false, t0.Add(30*time.Second+beatDebounce), 5*time.Minute)
	if !send || want {
		t.Errorf("a sustained fault should report down: send=%v want=%v", send, want)
	}
}

func TestDecideRateLimitsAFlap(t *testing.T) {
	var b beatState
	t0 := time.Now()
	b.decide(true, t0, 5*time.Minute)
	b.sent(true, t0)
	b.lastBeatAt = t0

	// Confirmed down.
	b.decide(false, t0.Add(time.Second), 5*time.Minute)
	send, want := b.decide(false, t0.Add(time.Second+beatDebounce), 5*time.Minute)
	if !send || want {
		t.Fatalf("expected a down beat: send=%v want=%v", send, want)
	}
	down := t0.Add(time.Second + beatDebounce)
	b.sent(false, down)
	b.lastBeatAt = down

	// Straight back up, inside the gap: held.
	if send, _ := b.decide(true, down.Add(5*time.Second), 5*time.Minute); send {
		t.Error("a recovery inside beatMinGap should be held back")
	}
	// Once the gap has passed it goes out.
	if send, want := b.decide(true, down.Add(beatMinGap+time.Second), 5*time.Minute); !send || !want {
		t.Errorf("recovery should push after the gap: send=%v want=%v", send, want)
	}
}

func TestDecidePushesOnTheInterval(t *testing.T) {
	var b beatState
	t0 := time.Now()
	b.decide(true, t0, 5*time.Minute)
	b.sent(true, t0)
	b.lastBeatAt = t0

	if send, _ := b.decide(true, t0.Add(4*time.Minute), 5*time.Minute); send {
		t.Error("pushed before the interval was up")
	}
	if send, want := b.decide(true, t0.Add(5*time.Minute), 5*time.Minute); !send || !want {
		t.Errorf("interval beat: send=%v want=%v", send, want)
	}
}
