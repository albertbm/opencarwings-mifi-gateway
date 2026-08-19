// opencarwings SMS gateway that runs on the modem itself. It opens /dev/appvcom1
// locally, connects to the opencarwings websocket, decrypts the commands and sends
// the SMS PDU over AT. This is a Go port of developerfromjokela/opencarwings-sms,
// built as one static binary so it can run on the MiFi with no host machine.
//
// The Balong firmware exposes two AT channels: /dev/appvcom and /dev/appvcom1.
// The HiLink daemon needs /dev/appvcom to poll the radio and keep the cellular
// data connection up, so the gateway must use the second channel. If it grabs
// /dev/appvcom instead, HiLink goes blind, never dials, and the modem loses its
// data connection, which also kills the gateway's own link to the server.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed ca-certificates.crt
var caPEM []byte

// Defaults; each is overridable by the matching environment variable so the same
// binary works against a self-hosted server or a different modem's AT device.
const (
	defaultWSURL    = "wss://opencarwings.viaaq.eu/ws/smsgateway/" // OCW_WS_URL
	defaultAppvcom  = "/dev/appvcom1"                              // OCW_APPVCOM (appvcom is reserved for HiLink)
	defaultIDsPath  = "/online/ocw_gw.ids"                         // OCW_IDS
	defaultConfPath = "/online/ocw_gw.conf"                        // OCW_CONF
	cmdTOms         = 5000
	cmgsTOms        = 20000
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---- runtime config (server url + enable flag), editable from the web page ----

type config struct {
	mu      sync.Mutex
	wsURL   string
	enabled bool
	path    string
}

var cfg = &config{}

// wake nudges the websocket loop when config changes so it reconnects promptly.
var wake = make(chan struct{}, 1)

// activeConn holds the current socket so a config change can close it and reconnect.
var activeConn struct {
	mu sync.Mutex
	c  *websocket.Conn
}

func setActive(c *websocket.Conn) {
	activeConn.mu.Lock()
	activeConn.c = c
	activeConn.mu.Unlock()
}

func closeActive() {
	activeConn.mu.Lock()
	if activeConn.c != nil {
		activeConn.c.Close()
	}
	activeConn.mu.Unlock()
}

func signalWake() {
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (c *config) get() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wsURL, c.enabled
}

func (c *config) load() {
	b, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var v struct {
		WSURL   string `json:"ws_url"`
		Enabled *bool  `json:"enabled"`
	}
	if json.Unmarshal(b, &v) != nil {
		return
	}
	c.mu.Lock()
	if v.WSURL != "" {
		c.wsURL = v.WSURL
	}
	if v.Enabled != nil {
		c.enabled = *v.Enabled
	}
	c.mu.Unlock()
}

func (c *config) save() {
	c.mu.Lock()
	v := map[string]any{"ws_url": c.wsURL, "enabled": c.enabled}
	p := c.path
	c.mu.Unlock()
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(p, b, 0600); err != nil {
		log.Printf("could not save config to %s: %v", p, err)
	}
}

// ---- boot autostart, installable from the web page (we run as root) ----

const (
	autorunPath   = "/system/etc/autorun.sh"
	autorunMarker = "# opencarwings gateway"
)

func autostartInstalled() bool {
	b, err := os.ReadFile(autorunPath)
	return err == nil && strings.Contains(string(b), autorunMarker)
}

// installAutostart appends a launch line to the modem boot script. /system is a
// read-only jffs2 partition at runtime, so we remount it rw, write, and put it back.
func installAutostart() error {
	if autostartInstalled() {
		return nil
	}
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "/online/ocwgw"
	}
	if err := syscall.Mount("", "/system", "", syscall.MS_REMOUNT, ""); err != nil {
		return fmt.Errorf("remount /system rw: %w", err)
	}
	defer syscall.Mount("", "/system", "", syscall.MS_REMOUNT|syscall.MS_RDONLY, "")
	f, err := os.OpenFile(autorunPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", autorunPath, err)
	}
	defer f.Close()
	line := fmt.Sprintf("\n%s\n[ -x %s ] && ( sleep 20; %s > /online/ocwgw.log 2>&1 ) &\n", autorunMarker, exe, exe)
	_, err = f.WriteString(line)
	return err
}

// ---- live status + built-in status web page ----

type gwStatus struct {
	mu        sync.Mutex
	deviceID  string
	keyHex    string
	wsState   string
	atOK      bool
	started   time.Time
	sentOK    int
	sentErr   int
	lastEvent string
	lastSent  string
}

var st = &gwStatus{started: time.Now(), wsState: "starting"}

func (s *gwStatus) setWS(v string)  { s.mu.Lock(); s.wsState = v; s.mu.Unlock() }
func (s *gwStatus) event(v string)  { s.mu.Lock(); s.lastEvent = v; s.mu.Unlock() }
func (s *gwStatus) sent(ok bool, v string) {
	s.mu.Lock()
	if ok {
		s.sentOK++
	} else {
		s.sentErr++
	}
	s.lastSent = v
	s.mu.Unlock()
}

func cls(ok bool) string {
	if ok {
		return "ok"
	}
	return "bad"
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

const pageHTML = `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>opencarwings gateway</title><style>
body{font-family:system-ui,sans-serif;max-width:640px;margin:24px auto;padding:0 16px;color:#111}
h1{font-size:1.3rem}code{background:#f2f2f2;padding:2px 6px;border-radius:4px;word-break:break-all}
.k{color:#555;width:150px;display:inline-block;vertical-align:top}
.ok{color:#0a7a0a;font-weight:600}.bad{color:#b00;font-weight:600}
.box{border:1px solid #ddd;border-radius:8px;padding:12px 16px;margin:12px 0}
.reg{background:#fff8e1;border-color:#e0c060}
input{padding:4px 6px;font-size:.95rem}button{padding:4px 12px;font-size:.95rem;cursor:pointer}
</style></head><body>
<h1>opencarwings MiFi gateway</h1>
<div class="box reg"><b>Register these on your opencarwings provider:</b><br>
<span class="k">device_id</span> <code>%s</code><br>
<span class="k">encryption_key</span> <code>%s</code></div>
<div class="box">
<span class="k">WebSocket</span> <span class="%s">%s</span><br>
<span class="k">Modem AT</span> <span class="%s">%s</span><br>
<span class="k">Sent ok / errors</span> %d / %d<br>
<span class="k">Last event</span> %s<br>
<span class="k">Last send</span> <code>%s</code><br>
<span class="k">Uptime</span> %s</div>
<div class="box">
<form method="post" action="/config">
<span class="k">Server URL</span> <input name="ws_url" value="%s" style="width:58%%"> <button>Save</button>
</form>
<form method="post" action="/toggle" style="margin-top:10px">
<span class="k">Gateway</span> <b>%s</b> &nbsp; <button>%s</button>
</form>
<div style="margin-top:10px">%s</div></div>
<p style="color:#888;font-size:.85rem">auto-refresh 5s &middot; <a href="/status.json">status.json</a></p>
<script>setTimeout(function(){location.reload()},5000)</script></body></html>`

func startHTTP(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status.json", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		v := map[string]any{
			"device_id": st.deviceID, "encryption_key": st.keyHex, "ws_state": st.wsState,
			"at_ok": st.atOK, "sent_ok": st.sentOK, "sent_err": st.sentErr,
			"last_event": st.lastEvent, "last_send": st.lastSent,
			"uptime_sec": int(time.Since(st.started).Seconds()),
		}
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		wsURL, enabled := cfg.get()
		enabledTxt, toggleLabel := "disabled", "Enable"
		if enabled {
			enabledTxt, toggleLabel = "enabled", "Disable"
		}
		autostart := `<span class="k">Autostart</span> <span class="ok">installed</span>`
		if !autostartInstalled() {
			autostart = `<span class="k">Autostart</span> <span class="bad">not installed</span> ` +
				`<form method="post" action="/install-autostart" style="display:inline;margin:0"><button>Install</button></form>`
		}
		st.mu.Lock()
		connected := strings.Contains(st.wsState, "connected")
		atTxt := "not responding"
		if st.atOK {
			atTxt = "OK"
		}
		body := fmt.Sprintf(pageHTML,
			html.EscapeString(st.deviceID), html.EscapeString(st.keyHex),
			cls(connected), html.EscapeString(st.wsState),
			cls(st.atOK), atTxt, st.sentOK, st.sentErr,
			html.EscapeString(or(st.lastEvent, "none")), html.EscapeString(or(st.lastSent, "none")),
			time.Since(st.started).Truncate(time.Second).String(),
			html.EscapeString(wsURL), enabledTxt, toggleLabel, autostart)
		st.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	})
	mux.HandleFunc("/install-autostart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := installAutostart(); err != nil {
				log.Printf("[autostart] install failed: %v", err)
			} else {
				log.Printf("[autostart] installed into %s", autorunPath)
			}
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			u := strings.TrimSpace(r.FormValue("ws_url"))
			if u != "" {
				cfg.mu.Lock()
				cfg.wsURL = u
				cfg.mu.Unlock()
				cfg.save()
				log.Printf("[cfg] server url set to %s", u)
				closeActive()
				signalWake()
			}
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("/toggle", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			cfg.mu.Lock()
			cfg.enabled = !cfg.enabled
			now := cfg.enabled
			cfg.mu.Unlock()
			cfg.save()
			log.Printf("[cfg] gateway %s", map[bool]string{true: "enabled", false: "disabled"}[now])
			closeActive()
			signalWake()
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("[http] server error: %v", err)
		}
	}()
	log.Printf("[http] status page on %s", addr)
}

// ---- identity (persisted) ----

type ids struct {
	DeviceID string `json:"device_id"` // 16 hex chars
	KeyHex   string `json:"key"`       // 32 hex chars (16 bytes)
}

func loadOrCreateIDs(path string) (string, []byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		var v ids
		if json.Unmarshal(b, &v) == nil && len(v.DeviceID) == 16 {
			key, err := hex.DecodeString(v.KeyHex)
			if err == nil && len(key) == 16 {
				return v.DeviceID, key, nil
			}
		}
	}
	did := make([]byte, 8)
	key := make([]byte, 16)
	if _, err := rand.Read(did); err != nil {
		return "", nil, err
	}
	if _, err := rand.Read(key); err != nil {
		return "", nil, err
	}
	v := ids{DeviceID: strings.ToLower(hex.EncodeToString(did)), KeyHex: hex.EncodeToString(key)}
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0600); err != nil {
		log.Printf("warning: could not persist ids to %s: %v", path, err)
	}
	return v.DeviceID, key, nil
}

// ---- AES-128-CBC / PKCS5 decrypt ([16 IV][ciphertext]) ----

func decrypt(data, key []byte) ([]byte, error) {
	if len(data) < 32 || len(data)%16 != 0 {
		return nil, errors.New("ciphertext too short / not block-aligned")
	}
	iv := data[:16]
	ct := data[16:]
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, ct)
	// strip PKCS5/7 padding
	if len(out) == 0 {
		return nil, errors.New("empty plaintext")
	}
	pad := int(out[len(out)-1])
	if pad < 1 || pad > 16 || pad > len(out) {
		return nil, errors.New("bad padding")
	}
	return out[:len(out)-pad], nil
}

// ---- modem over /dev/appvcom ----

type modem struct {
	rfd  int // raw read fd. Go's os.File poller won't read this char device, so we use syscalls
	wfd  int // raw write fd
	cmd  sync.Mutex // serialize command/response
	mu   sync.Mutex
	buf  []byte
}

func openModem(dev string) (*modem, error) {
	rfd, err := syscall.Open(dev, syscall.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	wfd, err := syscall.Open(dev, syscall.O_WRONLY, 0)
	if err != nil {
		syscall.Close(rfd)
		return nil, err
	}
	m := &modem{rfd: rfd, wfd: wfd}
	go m.reader()
	return m, nil
}

func (m *modem) reader() {
	b := make([]byte, 4096)
	for {
		n, err := syscall.Read(m.rfd, b)
		if n > 0 {
			m.mu.Lock()
			m.buf = append(m.buf, b[:n]...)
			if len(m.buf) > 1<<20 { // cap
				m.buf = m.buf[len(m.buf)-(1<<16):]
			}
			m.mu.Unlock()
		}
		if err == syscall.EINTR {
			continue
		}
		if err != nil || n == 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func (m *modem) mark() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.buf)
}

func (m *modem) since(mark int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mark > len(m.buf) {
		mark = len(m.buf)
	}
	return string(m.buf[mark:])
}

func (m *modem) write(s string) error {
	_, err := syscall.Write(m.wfd, []byte(s))
	return err
}

func (m *modem) readUntil(mark int, pred func(string) bool, timeoutMs int) (string, error) {
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		s := m.since(mark)
		if pred(s) {
			return s, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return m.since(mark), fmt.Errorf("timeout waiting for modem response")
}

func hasTerminator(b string) bool {
	return strings.Contains(b, "\r\nOK\r\n") || strings.Contains(b, "\r\nERROR\r\n") ||
		strings.Contains(b, "+CMS ERROR") || strings.Contains(b, "+CME ERROR")
}

func (m *modem) sendCommand(cmd string, timeoutMs int) (string, error) {
	mk := m.mark()
	if err := m.write(cmd + "\r"); err != nil {
		return "", err
	}
	b, err := m.readUntil(mk, hasTerminator, timeoutMs)
	if err != nil {
		return b, err
	}
	if !strings.Contains(b, "\r\nOK\r\n") {
		return b, fmt.Errorf("modem rejected %q: %s", cmd, strings.TrimSpace(b))
	}
	return b, nil
}

// health runs a plain AT under the command lock so it doesn't cut into a send.
// appvcom sometimes answers slower than one timeout, so try a few times before
// calling it down. That stops the status from flapping on a single slow reply.
func (m *modem) health() bool {
	m.cmd.Lock()
	defer m.cmd.Unlock()
	for i := 0; i < 3; i++ {
		if _, err := m.sendCommand("AT", 3000); err == nil {
			return true
		}
	}
	return false
}

func computeTpduLength(pdu string) int {
	total := len(pdu) / 2
	smscLen, _ := strconv.ParseInt(pdu[0:2], 16, 32)
	return total - (1 + int(smscLen))
}

func hasCmgsResult(b string) bool {
	ok := strings.Contains(b, "+CMGS:") && strings.Contains(b, "\r\nOK\r\n")
	return ok || strings.Contains(b, "+CMS ERROR") || strings.Contains(b, "+CME ERROR") ||
		strings.Contains(b, "\r\nERROR\r\n")
}

// sendAttempts is how many times a send is tried before giving up. appvcom can
// drop a prompt now and then, and a second try almost always goes through.
const sendAttempts = 2

// abortPrompt cancels any half-open SMS prompt and resyncs the channel between
// retries. Best effort, errors are ignored on purpose.
func (m *modem) abortPrompt() {
	m.write("\x1b")
	m.sendCommand("AT", cmdTOms)
}

func (m *modem) sendPdu(pduHex string, tpduLength *int) (string, error) {
	m.cmd.Lock()
	defer m.cmd.Unlock()
	var out string
	var err error
	for attempt := 1; attempt <= sendAttempts; attempt++ {
		if attempt > 1 {
			m.abortPrompt()
		}
		out, err = m.sendPduOnce(pduHex, tpduLength)
		if err == nil {
			return out, nil
		}
	}
	return out, err
}

func (m *modem) sendPduOnce(pduHex string, tpduLength *int) (string, error) {
	clean := strings.ToUpper(strings.TrimSpace(pduHex))
	if clean == "" || len(clean)%2 != 0 {
		return "", errors.New("bad PDU hex")
	}
	length := 0
	if tpduLength != nil {
		length = *tpduLength
	} else {
		length = computeTpduLength(clean)
	}
	if length <= 0 {
		return "", errors.New("bad tpduLength")
	}
	if _, err := m.sendCommand("AT+CMGF=0", cmdTOms); err != nil {
		return "", err
	}
	mk := m.mark()
	if err := m.write("AT+CMGS=" + strconv.Itoa(length) + "\r"); err != nil {
		return "", err
	}
	pb, err := m.readUntil(mk, func(b string) bool { return strings.Contains(b, ">") || strings.Contains(b, "ERROR") }, cmdTOms)
	if err != nil || !strings.Contains(pb, ">") {
		return pb, fmt.Errorf("no '>' prompt for AT+CMGS: %s", strings.TrimSpace(pb))
	}
	mk = m.mark()
	if err := m.write(clean + "\x1a"); err != nil {
		return "", err
	}
	b, err := m.readUntil(mk, hasCmgsResult, cmgsTOms)
	if err != nil {
		return b, err
	}
	if !strings.Contains(b, "+CMGS:") || !strings.Contains(b, "\r\nOK\r\n") {
		return b, fmt.Errorf("send failed: %s", strings.TrimSpace(b))
	}
	return strings.TrimSpace(b), nil
}

func (m *modem) sendText(phone, text string) (string, error) {
	m.cmd.Lock()
	defer m.cmd.Unlock()
	var out string
	var err error
	for attempt := 1; attempt <= sendAttempts; attempt++ {
		if attempt > 1 {
			m.abortPrompt()
		}
		out, err = m.sendTextOnce(phone, text)
		if err == nil {
			return out, nil
		}
	}
	return out, err
}

func (m *modem) sendTextOnce(phone, text string) (string, error) {
	if _, err := m.sendCommand("AT+CMGF=1", cmdTOms); err != nil {
		return "", err
	}
	mk := m.mark()
	if err := m.write("AT+CMGS=\"" + phone + "\"\r"); err != nil {
		return "", err
	}
	pb, err := m.readUntil(mk, func(b string) bool { return strings.Contains(b, ">") || strings.Contains(b, "ERROR") }, cmdTOms)
	if err != nil || !strings.Contains(pb, ">") {
		return pb, fmt.Errorf("no '>' prompt: %s", strings.TrimSpace(pb))
	}
	mk = m.mark()
	if err := m.write(text + "\x1a"); err != nil {
		return "", err
	}
	b, err := m.readUntil(mk, hasCmgsResult, cmgsTOms)
	if err != nil {
		return b, err
	}
	if !strings.Contains(b, "+CMGS:") || !strings.Contains(b, "\r\nOK\r\n") {
		return b, fmt.Errorf("send failed: %s", strings.TrimSpace(b))
	}
	return strings.TrimSpace(b), nil
}

// ---- websocket loop ----

type srvMsg struct {
	Type   string `json:"type"`
	Pdu    string `json:"pdu"`
	Length *int   `json:"length"`
	Sms    string `json:"sms"`
	Phone  string `json:"phone"`
}

func handle(m *modem, key, payload []byte) {
	plain, err := decrypt(payload, key)
	if err != nil {
		log.Printf("[ws] decrypt failed: %v", err)
		return
	}
	var msg srvMsg
	if err := json.Unmarshal(plain, &msg); err != nil {
		log.Printf("[ws] bad json: %v (%q)", err, string(plain))
		return
	}
	switch msg.Type {
	case "connect":
		log.Printf("[ws] connected!")
		st.event("server acknowledged connection")
	case "pdu":
		pduLen := -1
		if msg.Length != nil {
			pduLen = *msg.Length
		}
		log.Printf("[ws] PDU: %s (len=%d)", msg.Pdu, pduLen)
		st.event("PDU command received")
		res, err := m.sendPdu(msg.Pdu, msg.Length)
		if err != nil {
			log.Printf("[sms] PDU send error: %v", err)
			st.sent(false, "PDU error: "+err.Error())
		} else {
			log.Printf("[sms] sent: %s", res)
			st.sent(true, "PDU sent")
		}
	case "sms":
		log.Printf("[ws] SMS to %s: %s", msg.Phone, msg.Sms)
		st.event("SMS command received")
		res, err := m.sendText(msg.Phone, msg.Sms)
		if err != nil {
			log.Printf("[sms] text send error: %v", err)
			st.sent(false, "SMS error: "+err.Error())
		} else {
			log.Printf("[sms] sent: %s", res)
			st.sent(true, "SMS to "+msg.Phone)
		}
	default:
		log.Printf("[ws] unknown type: %s", msg.Type)
		st.event("unknown server type: " + msg.Type)
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	idPath := env("OCW_IDS", defaultIDsPath)
	appvcomDev := env("OCW_APPVCOM", defaultAppvcom)
	wsURL := env("OCW_WS_URL", defaultWSURL)
	deviceID, key, err := loadOrCreateIDs(idPath)
	if err != nil {
		log.Fatalf("ids: %v", err)
	}
	fmt.Println("=======================================================")
	fmt.Println(" opencarwings native gateway")
	fmt.Println("   device_id      :", deviceID)
	fmt.Println("   encryption_key :", hex.EncodeToString(key))
	fmt.Println("   >>> register BOTH on opencarwings.viaaq.eu <<<")
	fmt.Println("=======================================================")

	st.mu.Lock()
	st.deviceID = deviceID
	st.keyHex = hex.EncodeToString(key)
	st.mu.Unlock()

	// Runtime config: start from env/defaults, then load saved overrides. This is
	// what the web page edits (server url and the enable switch).
	cfg.path = env("OCW_CONF", defaultConfPath)
	cfg.wsURL = wsURL
	cfg.enabled = true
	cfg.load()

	startHTTP(env("OCW_HTTP_ADDR", ":8080"))

	// Wait for the modem AT device. On a cold boot it may not be there yet, so we
	// keep trying instead of giving up. The web page is already serving by now.
	var m *modem
	for {
		mm, err := openModem(appvcomDev)
		if err == nil {
			m = mm
			break
		}
		log.Printf("waiting for %s: %v", appvcomDev, err)
		time.Sleep(5 * time.Second)
	}

	// Keep the modem AT status current so the page reflects reality and recovers on
	// its own if a check happens to time out.
	go func() {
		logged := false
		for {
			ok := m.health()
			st.mu.Lock()
			st.atOK = ok
			st.mu.Unlock()
			if !logged {
				if ok {
					log.Printf("modem AT channel OK on %s", appvcomDev)
				} else {
					log.Printf("modem AT channel not responding on %s (will keep checking)", appvcomDev)
				}
				logged = true
			}
			time.Sleep(30 * time.Second)
		}
	}()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		log.Printf("warning: embedded CA bundle failed to load")
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		TLSClientConfig:  &tls.Config{RootCAs: pool}, // verify against embedded CAs
	}

	waitOrWake := func(d time.Duration) {
		select {
		case <-wake:
		case <-time.After(d):
		}
	}

	delay := 2 * time.Second
	for {
		curURL, enabled := cfg.get()
		if !enabled {
			st.setWS("disabled")
			waitOrWake(10 * time.Second)
			continue
		}

		u, err := url.Parse(curURL)
		if err != nil {
			st.setWS("bad server url")
			log.Printf("[ws] bad server url %q: %v", curURL, err)
			waitOrWake(10 * time.Second)
			continue
		}
		q := u.Query()
		q.Set("device_id", deviceID)
		u.RawQuery = q.Encode()

		st.setWS("connecting")
		log.Printf("[ws] connecting to %s", u.String())
		c, _, err := dialer.Dial(u.String(), nil)
		if err != nil {
			if strings.Contains(err.Error(), "bad handshake") {
				st.setWS("rejected, is device_id registered on the server?")
			} else {
				st.setWS("disconnected")
			}
			log.Printf("[ws] dial failed: %v; retry in %s", err, delay)
			select {
			case <-wake:
				delay = 2 * time.Second // config changed, retry now
			case <-time.After(delay):
				if delay < 60*time.Second {
					delay *= 2
				}
			}
			continue
		}
		delay = 2 * time.Second
		setActive(c)
		st.setWS("connected")
		log.Printf("[ws] socket open")
		c.SetPongHandler(func(string) error { return nil })
		done := make(chan struct{})
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					c.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
				}
			}
		}()
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				log.Printf("[ws] read error: %v", err)
				break
			}
			if mt == websocket.BinaryMessage {
				handle(m, key, data)
			} else {
				log.Printf("[ws] text message: %s", string(data))
			}
		}
		close(done)
		setActive(nil)
		c.Close()
		st.setWS("disconnected")
		time.Sleep(time.Second)
	}
}
