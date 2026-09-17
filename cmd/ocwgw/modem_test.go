package main

import (
	"strings"
	"syscall"
	"testing"
	"time"
)

// newFakeModem wires the modem up to a pair of pipes instead of /dev/appvcom1.
// Writing to modemOut is the radio talking; anything the gateway sends lands in
// the other pipe and is ignored unless a test reads it.
func newFakeModem(t *testing.T) (m *modem, modemOut int) {
	t.Helper()
	var toGw, toModem [2]int
	if err := syscall.Pipe(toGw[:]); err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := syscall.Pipe(toModem[:]); err != nil {
		t.Fatalf("pipe: %v", err)
	}
	m = &modem{rfd: toGw[0], wfd: toModem[1]}
	go m.reader()
	return m, toGw[1]
}

func radioSays(t *testing.T, fd int, s string) {
	t.Helper()
	if _, err := syscall.Write(fd, []byte(s)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestStripUnsolicited(t *testing.T) {
	cases := []struct {
		name string
		in   string
		tag  string
		want string
	}{
		{"nothing to strip", "\r\nOK\r\n", "", "\r\nOK\r\n"},
		{
			"dsflowrpt around the terminator",
			"\r\n^DSFLOWRPT:00026F12,00000000,00000000\r\n\r\nOK\r\n",
			"",
			"\r\n\r\nOK\r\n",
		},
		{
			"signal reports mixed into a response",
			"\r\n^RSSI:20\r\n\r\n+CSCA: \"+3546999099\",145\r\n\r\n^HCSQ:\"LTE\",48,44\r\n\r\nOK\r\n",
			"",
			"\r\n\r\n+CSCA: \"+3546999099\",145\r\n\r\n\r\nOK\r\n",
		},
		{
			"the answer to our own ^ command stays",
			"\r\n^DSFLOWRPT:0001,0,0\r\n\r\n^DSFLOWQRY:001A,0D49,2BE3\r\n\r\nOK\r\n",
			"^DSFLOWQRY:",
			"\r\n\r\n^DSFLOWQRY:001A,0D49,2BE3\r\n\r\nOK\r\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripUnsolicited(c.in, c.tag); got != c.want {
				t.Errorf("stripUnsolicited(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

// A response is still recognised when the radio interleaves its own chatter.
func TestTerminatorSurvivesUnsolicited(t *testing.T) {
	noisy := stripUnsolicited("\r\n^DSFLOWRPT:0001,0,0\r\n\r\nOK\r\n", "")
	if !hasTerminator(noisy) {
		t.Fatalf("terminator lost after stripping: %q", noisy)
	}
	if !strings.Contains(noisy, "\r\nOK\r\n") {
		t.Errorf("want \\r\\nOK\\r\\n in %q", noisy)
	}
}

// The regression this fix is for. A send's +CMS ERROR arrives after that send's
// own read already gave up, so it lands inside the window of the retry's
// AT+CMGF=0 — after the mark, which is why marking alone never prevented it.
// It used to satisfy the terminator check and be reported as
// `modem rejected "AT+CMGF=0"` for a command the modem went on to accept.
func TestLateSendErrorNotBlamedOnNextCommand(t *testing.T) {
	m, out := newFakeModem(t)

	go func() {
		time.Sleep(300 * time.Millisecond) // after AT+CMGF=0 has gone out
		radioSays(t, out, "\r\n^DSFLOWRPT:00026F12,00000000,00000000\r\n\r\n+CMS ERROR: 38\r\n")
		time.Sleep(300 * time.Millisecond)
		radioSays(t, out, "\r\nOK\r\n") // the modem's actual answer to AT+CMGF=0
	}()

	got, err := m.sendCommand("AT+CMGF=0", cmdTOms)
	if err != nil {
		t.Fatalf("AT+CMGF=0 blamed for a late send error: %v (window %q)", err, got)
	}
}

// The counterpart: a real rejection must still be reported as one, so the fix
// above cannot be mistaken for "never fail".
func TestRealRejectionStillFails(t *testing.T) {
	m, out := newFakeModem(t)

	go func() {
		time.Sleep(300 * time.Millisecond)
		radioSays(t, out, "\r\nERROR\r\n")
	}()

	if _, err := m.sendCommand("AT+CMGF=0", cmdTOms); err == nil {
		t.Fatal("want an error for a modem that answered ERROR, got nil")
	}
}

// Unsolicited reports arriving while a command is in flight must not be mistaken
// for its response.
func TestUnsolicitedDuringCommand(t *testing.T) {
	m, out := newFakeModem(t)

	go func() {
		time.Sleep(700 * time.Millisecond)
		radioSays(t, out, "\r\n^RSSI:20\r\n")
		time.Sleep(100 * time.Millisecond)
		radioSays(t, out, "\r\n^HCSQ:\"LTE\",48,44,131,26\r\n")
		time.Sleep(100 * time.Millisecond)
		radioSays(t, out, "\r\nOK\r\n")
	}()

	if _, err := m.sendCommand("AT", cmdTOms); err != nil {
		t.Fatalf("AT failed with unsolicited reports in the window: %v", err)
	}
}

// drain must skip whatever is already buffered and return a mark past it.
func TestDrainSkipsBufferedBytes(t *testing.T) {
	m, out := newFakeModem(t)

	radioSays(t, out, "\r\n+CMS ERROR: 38\r\n")
	time.Sleep(200 * time.Millisecond)

	mk := m.drain(drainQuietMs, drainMaxMs)
	if s := m.since(mk); s != "" {
		t.Errorf("drain left %q in the window", s)
	}

	radioSays(t, out, "\r\nOK\r\n")
	time.Sleep(200 * time.Millisecond)
	if s := m.since(mk); !strings.Contains(s, "\r\nOK\r\n") {
		t.Errorf("post-drain data missing, got %q", s)
	}
}
