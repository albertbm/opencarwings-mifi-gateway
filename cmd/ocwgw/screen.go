// The MiFi's front panel is a plain framebuffer: /dev/graphics/fb0, 128x128 at
// 16 bpp, two pages inside a 128x256 virtual buffer. Pixels reach the panel on
// FBIOPAN_DISPLAY, so a frame ends with a page flip.
//
// The stock `oled` process draws the Huawei UI into the same buffer and flips
// pages on its own clock. Sharing it means both sides clobber each other and the
// screen blinks, so the gateway takes the panel over: it stops `oled`, draws the
// whole frame itself, and hands it back on the menu button or on the way out.
// Nothing else then drives the backlight or listens to the buttons, so we do both.
//
// The modem needs its cycles for the radio, so the screen costs as little as it
// can: nothing at all while the panel is dark, readings on a timer rather than
// per frame, drawing straight into the page rather than into an image we would
// then convert, and no repaint when the frame would come out the same as the one
// already up.
//
// The panel wants its pixels big-endian, whatever the driver's bitfields claim.

package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/png"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/image/font/basicfont"
)

//go:embed ocw-logo.png
var logoPNG []byte

const (
	fbDev         = "/dev/graphics/fb0"
	backlightPath = "/sys/class/leds/lcd-backlight/brightness"
	batteryPath   = "/sys/class/power_supply/battery"
	keyDev        = "/dev/input/event0"
	// One file per access point, listing the stations associated with it.
	stationInfoGlob = "/var/ap*_stainfo"
	// The firmware's own web API, for the unread message count. It has to be
	// asked on the LAN address: a request to loopback gets the index page.
	hilinkNotify       = "/api/monitoring/check-notifications"
	hilinkFallbackHost = "192.168.8.1"
	lanIface           = "br0"

	fbioGetVScreenInfo = 0x4600
	fbioPanDisplay     = 0x4606

	screenFrame = time.Second      // tick while the panel is lit
	screenWake  = 20 * time.Second // how long a button press keeps it lit
	askWindow   = 6 * time.Second  // to answer the stop/start question
	pollEvery   = 15 * time.Second // radio, battery, clients, unread
	stockIdle   = 25 * time.Second // the stock UI keeps the panel this long after a press
	brightness  = 200              // of 255

	// The bezel covers the first rows and the last few, which is why the stock
	// UI starts its own icons at 6. Everything we draw lives between these.
	safeTop = 6
	safeBot = 120

	topBarH = 15 // the stock status icons, at their stock positions
	botBarH = 28 // gateway status, two lines

	keyPower = 116
	keyMenu  = 139
)

// Frame numbers of the stock mode_type icon. Only a few of them draw differently.
const (
	modeFrame2G = 1
	modeFrame3G = 5
	modeFrame4G = 12
)

var (
	lanOnce sync.Once
	lanAddr string

	macAddr    = regexp.MustCompile(`[0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5}`)
	unreadSMSs = regexp.MustCompile(`<UnreadMessage>(\d+)</UnreadMessage>`)
)

// The modem is opened after the screen starts, so the readings pick it up
// whenever it turns up.
var atModem atomic.Pointer[modem]

// frameReq asks the screen loop for a shot of what the panel would be showing.
// Nothing is kept between requests: the loop draws one frame and answers.
var frameReq = make(chan chan *image.RGBA)

func rgb565(r, g, b uint8) uint16 {
	return uint16(r>>3)<<11 | uint16(g>>2)<<5 | uint16(b>>3)
}

var (
	colBG   uint16 = 0
	colText        = rgb565(235, 235, 235)
	colDim         = rgb565(130, 130, 130)
	colRule        = rgb565(50, 50, 50)
	colBad         = rgb565(255, 70, 50)
)

type fbBitfield struct{ Offset, Length, MSBRight uint32 }

// fb_var_screeninfo as the 32-bit ARM kernel lays it out.
type fbVarInfo struct {
	Xres, Yres               uint32
	XresVirtual, YresVirtual uint32
	Xoffset, Yoffset         uint32
	BitsPerPixel, Grayscale  uint32
	Red, Green, Blue, Transp fbBitfield
	Nonstd, Activate         uint32
	Height, Width            uint32
	AccelFlags               uint32
	Pixclock                 uint32
	Margins                  [4]uint32
	HsyncLen, VsyncLen       uint32
	Sync, Vmode, Rotate      uint32
	Colorspace               uint32
	Reserved                 [4]uint32
}

type screen struct {
	f    *os.File
	mem  []byte
	vi   fbVarInfo
	w, h int
}

func openScreen() (*screen, error) {
	f, err := os.OpenFile(fbDev, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	s := &screen{f: f}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fbioGetVScreenInfo, uintptr(unsafe.Pointer(&s.vi))); e != 0 {
		f.Close()
		return nil, fmt.Errorf("FBIOGET_VSCREENINFO: %v", e)
	}
	if s.vi.BitsPerPixel != 16 {
		f.Close()
		return nil, fmt.Errorf("want 16 bpp, panel says %d", s.vi.BitsPerPixel)
	}
	if s.vi.YresVirtual < 2*s.vi.Yres {
		f.Close()
		return nil, fmt.Errorf("no second page: virtual %dx%d", s.vi.XresVirtual, s.vi.YresVirtual)
	}
	s.w, s.h = int(s.vi.Xres), int(s.vi.Yres)
	s.mem, err = syscall.Mmap(int(f.Fd()), 0, int(s.vi.XresVirtual*s.vi.YresVirtual*2),
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap: %w", err)
	}
	return s, nil
}

func (s *screen) close() {
	syscall.Munmap(s.mem)
	s.f.Close()
}

// back is the page that is not on screen, which is where a frame is drawn.
func (s *screen) back() []byte {
	page := s.w * s.h * 2
	if s.vi.Yoffset == 0 {
		return s.mem[page : 2*page]
	}
	return s.mem[:page]
}

// flip shows the page that was just drawn. The pan is what pushes the pixels
// down the SPI link, so a frame is not on the panel until this returns.
func (s *screen) flip() error {
	if s.vi.Yoffset == 0 {
		s.vi.Yoffset = uint32(s.h)
	} else {
		s.vi.Yoffset = 0
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, s.f.Fd(), fbioPanDisplay, uintptr(unsafe.Pointer(&s.vi))); e != 0 {
		return fmt.Errorf("FBIOPAN_DISPLAY: %v", e)
	}
	return nil
}

// ---- drawing, straight into a page ----

func (s *screen) set(buf []byte, x, y int, c uint16) {
	if uint(x) >= uint(s.w) || uint(y) >= uint(s.h) {
		return
	}
	i := (y*s.w + x) * 2
	buf[i] = byte(c >> 8)
	buf[i+1] = byte(c)
}

func (s *screen) clear(buf []byte) {
	for i := range buf {
		buf[i] = 0
	}
}

func (s *screen) rect(buf []byte, x0, y0, x1, y1 int, c uint16) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			s.set(buf, x, y, c)
		}
	}
}

// text draws with the 7x13 bitmap font, straight from its mask.
func (s *screen) text(buf []byte, x, baseline int, str string, c uint16) {
	f := basicfont.Face7x13
	m, ok := f.Mask.(*image.Alpha)
	if !ok {
		return
	}
	top := baseline - f.Ascent
	for _, r := range str {
		if g := glyphRow(r); g >= 0 {
			for gy := 0; gy < f.Height; gy++ {
				row := (m.Rect.Min.Y+g*f.Height+gy)*m.Stride + m.Rect.Min.X
				for gx := 0; gx < f.Width; gx++ {
					if m.Pix[row+gx] > 0x80 {
						s.set(buf, x+gx, top+gy, c)
					}
				}
			}
		}
		x += f.Width
	}
}

func (s *screen) textRight(buf []byte, right, baseline int, str string, c uint16) {
	s.text(buf, right-basicfont.Face7x13.Width*len(str), baseline, str, c)
}

// glyphRow is where a rune's bitmap starts in the font mask, counted in glyphs.
func glyphRow(r rune) int {
	for _, rg := range basicfont.Face7x13.Ranges {
		if r >= rg.Low && r < rg.High {
			return rg.Offset + int(r-rg.Low)
		}
	}
	return -1
}

// sprite is an image ready for the panel: pixels in its format, and which of
// them to draw. Converting once at startup keeps the per-frame cost to a copy.
type sprite struct {
	w, h int
	px   []uint16
	on   []bool
}

func loadSprite(b []byte) *sprite {
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		log.Printf("[screen] logo: %v", err)
		return nil
	}
	r := img.Bounds()
	sp := &sprite{w: r.Dx(), h: r.Dy()}
	sp.px = make([]uint16, sp.w*sp.h)
	sp.on = make([]bool, sp.w*sp.h)
	for y := 0; y < sp.h; y++ {
		for x := 0; x < sp.w; x++ {
			cr, cg, cb, a := img.At(r.Min.X+x, r.Min.Y+y).RGBA()
			if a == 0 {
				continue
			}
			i := y*sp.w + x
			// The PNG is premultiplied against black, which is what we draw on.
			sp.px[i] = rgb565(uint8(cr>>8), uint8(cg>>8), uint8(cb>>8))
			sp.on[i] = true
		}
	}
	return sp
}

func (s *screen) sprite(buf []byte, sp *sprite, x0, y0 int) {
	if sp == nil {
		return
	}
	for y := 0; y < sp.h; y++ {
		for x := 0; x < sp.w; x++ {
			if i := y*sp.w + x; sp.on[i] {
				s.set(buf, x0+x, y0+y, sp.px[i])
			}
		}
	}
}

// ---- the three bands of the frame ----

// topBar is the stock status row: signal, radio type, wifi and its client count,
// unread messages, battery. With no icon file to read we draw our own.
func (s *screen) topBar(buf []byte, set *iconSet, r readings) {
	if set == nil {
		s.plainTopBar(buf, r)
		return
	}
	s.icon(buf, set.Signal, r.bars, colText)
	s.icon(buf, set.Mode, modeFrame(r.mode), colText)
	s.icon(buf, set.Wlan, 0, colText)
	s.icon(buf, set.Users, r.clients, colText)
	if r.unread > 0 {
		s.icon(buf, set.Message, r.unread, colText)
	}
	c := colText
	if r.batt <= 15 && !r.charging {
		c = colBad
	}
	s.icon(buf, set.Battery, r.batt*(len(set.Battery.bits)-1)/100, c)
}

func modeFrame(mode string) int {
	switch {
	case strings.Contains(mode, "LTE"):
		return modeFrame4G
	case strings.Contains(mode, "WCDMA"), strings.Contains(mode, "HSPA"), strings.Contains(mode, "TD-SCDMA"):
		return modeFrame3G
	}
	return modeFrame2G
}

// plainTopBar is the fallback: five bars and a battery box.
func (s *screen) plainTopBar(buf []byte, r readings) {
	base := safeTop + topBarH - 2
	for i := 0; i < 5; i++ {
		c := colRule
		if i < r.bars {
			c = colText
		}
		s.rect(buf, 4+i*4, base-(2+2*i), 4+i*4+2, base, c)
	}
	const bw, bh = 14, 8
	x0, y0 := s.w-6-bw, base-bh
	s.rect(buf, x0, y0, x0+bw, y0+bh, colDim)
	s.rect(buf, x0+1, y0+1, x0+bw-1, y0+bh-1, colBG)
	s.rect(buf, x0+bw, y0+2, x0+bw+2, y0+bh-2, colDim)
	s.rect(buf, x0+2, y0+2, x0+2+(bw-4)*r.batt/100, y0+bh-2, colText)
}

// botBar is the gateway itself: state and data used on top, counters under it.
func (s *screen) botBar(buf []byte, f frame) {
	y := safeBot - botBarH
	s.rect(buf, 0, y, s.w, y+1, colRule)
	c := colText
	if !f.ok {
		c = colBad
	}
	s.text(buf, 4, y+13, f.state, c)
	s.textRight(buf, s.w-4, y+13, f.data, colDim)
	s.text(buf, 4, y+26, f.counters, colDim)
}

// askBar replaces the status bar with the question the power button opens. The
// menu button belongs to the stock UI, so the answer keys are the other way up.
func (s *screen) askBar(buf []byte, running bool) {
	y := safeBot - botBarH
	s.rect(buf, 0, y, s.w, safeBot, colText)
	q := "STOP GATEWAY?"
	if !running {
		q = "START GATEWAY?"
	}
	s.text(buf, 4, y+13, q, colBG)
	s.text(buf, 4, y+26, "power=yes menu=no", colBG)
}

// ---- what to put in it ----

// readings is everything on the screen that costs something to find out. It is
// refreshed on a timer, never per frame.
type readings struct {
	bars     int
	mode     string // LTE, WCDMA, GSM, as ^SYSINFOEX names it
	unread   int
	clients  int
	batt     int
	charging bool
	up, down uint64 // bytes since the data session came up
}

// poll takes one round of readings. The AT part runs under the command lock so
// it cannot cut into a send. All of it is cosmetic: a failure leaves the last
// reading on screen.
func poll(prev readings) readings {
	out := prev
	out.unread = unreadCount()
	out.clients = wifiClients()
	out.batt, out.charging = battery()

	m := atModem.Load()
	if m == nil {
		return out
	}
	m.cmd.Lock()
	defer m.cmd.Unlock()

	if b, err := m.sendCommand("AT+CSQ", 3000); err == nil {
		if rssi, err := strconv.Atoi(field(b, "+CSQ:", 0)); err == nil {
			out.bars = bars(rssi)
		}
	}
	// ^SYSINFOEX: ...,"LTE",101,"LTE" ends with the radio technology by name.
	if b, err := m.sendCommand("AT^SYSINFOEX", 3000); err == nil {
		if f := field(b, "^SYSINFOEX:", 8); f != "" {
			out.mode = f
		}
	}
	// ^DSFLOWQRY: <secs>,<tx>,<rx>,... in hex, for the current data session.
	if b, err := m.sendCommand("AT^DSFLOWQRY", 3000); err == nil {
		up, err1 := strconv.ParseUint(field(b, "^DSFLOWQRY:", 1), 16, 64)
		down, err2 := strconv.ParseUint(field(b, "^DSFLOWQRY:", 2), 16, 64)
		if err1 == nil && err2 == nil {
			out.up, out.down = up, down
		}
	}
	return out
}

// unreadCount asks the firmware's web API how many messages are waiting. The
// radio's own storage reads as empty: HiLink takes messages out of it into its
// own database, and the API is the only thing that says how many are unread.
func unreadCount() int {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + lanHost() + hilinkNotify)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var b [512]byte
	n, _ := resp.Body.Read(b[:])
	m := unreadSMSs.FindSubmatch(b[:n])
	if m == nil {
		return 0
	}
	v, _ := strconv.Atoi(string(m[1]))
	return v
}

// lanHost is the address the firmware serves its pages on.
func lanHost() string {
	lanOnce.Do(func() {
		lanAddr = hilinkFallbackHost
		ifi, err := net.InterfaceByName(lanIface)
		if err != nil {
			return
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			return
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				lanAddr = n.IP.String()
				return
			}
		}
	})
	return lanAddr
}

// wifiClients counts the stations the firmware lists for each access point. The
// files are fixed-size records with the MAC address in plain text, so counting
// the addresses saves having to know the layout.
func wifiClients() int {
	paths, _ := filepath.Glob(stationInfoGlob)
	seen := make(map[string]bool)
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, m := range macAddr.FindAll(b, -1) {
			if mac := strings.ToUpper(string(m)); mac != "00:00:00:00:00:00" {
				seen[mac] = true
			}
		}
	}
	return len(seen)
}

func battery() (int, bool) {
	b, err := os.ReadFile(filepath.Join(batteryPath, "capacity"))
	if err != nil {
		return 0, false
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	st, _ := os.ReadFile(filepath.Join(batteryPath, "status"))
	return n, strings.TrimSpace(string(st)) == "Charging"
}

// field returns comma-separated field n of the line starting with tag.
func field(b, tag string, n int) string {
	i := strings.Index(b, tag)
	if i < 0 {
		return ""
	}
	line := b[i+len(tag):]
	if j := strings.IndexAny(line, "\r\n"); j >= 0 {
		line = line[:j]
	}
	f := strings.Split(line, ",")
	if n >= len(f) {
		return ""
	}
	return strings.Trim(strings.TrimSpace(f[n]), `"`)
}

// bars maps the +CSQ RSSI onto five bars. A reading of 99 means the modem has no
// idea, which is not the same as no signal.
func bars(rssi int) int {
	switch {
	case rssi == 99:
		return 0
	case rssi >= 25:
		return 5
	case rssi >= 19:
		return 4
	case rssi >= 14:
		return 3
	case rssi >= 9:
		return 2
	case rssi >= 4:
		return 1
	}
	return 0
}

// humanBytes keeps it to five characters, which is what the corner has room for.
func humanBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%dM", n/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%dK", n/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// frame is everything a drawn frame depends on. Two equal frames look the same,
// so the second one is not worth drawing.
type frame struct {
	bars, mode, unread, clients, batt int
	charging, ok, ask, running        bool
	state, data, counters             string
}

// newFrame reads the gateway's own state. Eighteen characters fit per line.
func newFrame(r readings, ask bool) frame {
	st.mu.Lock()
	ws, atOK := st.wsState, st.atOK
	sentOK, sentErr := st.sentOK, st.sentErr
	st.mu.Unlock()
	_, running := cfg.get()

	f := frame{
		bars: r.bars, mode: modeFrame(r.mode), unread: r.unread, clients: r.clients,
		batt: r.batt, charging: r.charging, ask: ask, running: running,
		data:     humanBytes(r.up + r.down),
		counters: fmt.Sprintf("sent %d  err %d", sentOK, sentErr),
	}
	switch {
	case !atOK:
		f.state = "NO MODEM"
	case strings.HasPrefix(ws, "connected"):
		f.state, f.ok = "ONLINE", true
	case strings.HasPrefix(ws, "rejected"):
		f.state = "NOT REGD"
	case ws == "disabled":
		f.state = "STOPPED"
	case ws == "connecting":
		f.state = "CONNECTING"
	default:
		f.state = "NO LINK"
	}
	return f
}

func (s *screen) draw(buf []byte, set *iconSet, logo *sprite, r readings, f frame) {
	s.clear(buf)
	s.topBar(buf, set, r)
	if logo != nil {
		top, bot := safeTop+topBarH, safeBot-botBarH
		s.sprite(buf, logo, (s.w-logo.w)/2, top+(bot-top-logo.h)/2)
	}
	if f.ask {
		s.askBar(buf, f.running)
	} else {
		s.botBar(buf, f)
	}
}

// ---- the stock UI process, and the buttons it used to own ----

// findProcess returns the pid whose /proc/<pid>/exe ends in name.
func findProcess(name string) int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe")); err == nil &&
			filepath.Base(exe) == name {
			return pid
		}
	}
	return 0
}

func setBacklight(v int) {
	os.WriteFile(backlightPath, []byte(strconv.Itoa(v)), 0644)
}

// watchKeys reports every key press. The buttons belong to the stock UI, which
// is stopped while we hold the panel, so a press means someone is looking.
func watchKeys(press chan<- uint16) {
	fd, err := syscall.Open(keyDev, syscall.O_RDONLY, 0)
	if err != nil {
		log.Printf("[screen] no buttons: %v", err)
		return
	}
	defer syscall.Close(fd)
	// struct input_event on 32-bit ARM: timeval, type, code, value.
	b := make([]byte, 16)
	for {
		n, err := syscall.Read(fd, b)
		if err != nil {
			return
		}
		if n < 16 {
			continue
		}
		typ := *(*uint16)(unsafe.Pointer(&b[8]))
		code := *(*uint16)(unsafe.Pointer(&b[10]))
		val := *(*int32)(unsafe.Pointer(&b[12]))
		if typ == 1 && val == 1 { // EV_KEY, pressed
			select {
			case press <- code:
			default:
			}
		}
	}
}

// servePNG asks the screen loop for a shot of the panel. With no panel on this
// modem nothing answers, so the wait is short.
func servePNG(w http.ResponseWriter, r *http.Request) {
	reply := make(chan *image.RGBA, 1)
	select {
	case frameReq <- reply:
	case <-time.After(2 * time.Second):
		http.Error(w, "no panel on this modem", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	png.Encode(w, <-reply)
}

// toImage converts a page into something image/png can write out. Only the
// status page asks for this, so it stays off the drawing path.
func (s *screen) toImage(buf []byte) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, s.w, s.h))
	for i := 0; i < s.w*s.h; i++ {
		v := uint16(buf[i*2])<<8 | uint16(buf[i*2+1])
		img.Pix[i*4] = uint8(v>>11) << 3
		img.Pix[i*4+1] = uint8(v>>5&0x3F) << 2
		img.Pix[i*4+2] = uint8(v&0x1F) << 3
		img.Pix[i*4+3] = 0xFF
	}
	return img
}

// screenLoop draws the idle screen and shares the panel with the stock UI: the
// menu button hands `oled` back so its own menu, SMS list and data pages work as
// they always did, and the panel comes back to us once that goes idle. It gives
// up quietly on anything it cannot open, so the same binary still runs on a
// modem with no panel.
func screenLoop() {
	s, err := openScreen()
	if err != nil {
		log.Printf("[screen] off: %v", err)
		return
	}
	defer s.close()

	logo := loadSprite(logoPNG)
	icons := loadIcons(env("OCW_ICONS", iconXMLPath))

	oled := findProcess("oled")
	stock := false // true while the stock UI has the panel
	if oled > 0 {
		syscall.Kill(oled, syscall.SIGSTOP)
		log.Printf("[screen] stock UI is pid %d, paused while we hold the panel", oled)
	}

	// Whatever happens to us, the stock UI gets its panel back. A SIGKILL is the
	// one way out that leaves it stopped.
	restore := func() {
		if oled > 0 {
			syscall.Kill(oled, syscall.SIGCONT)
		}
	}
	defer restore()
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		restore()
		os.Exit(0)
	}()

	press := make(chan uint16, 1)
	go watchKeys(press)

	log.Printf("[screen] %dx%d on %s", s.w, s.h, fbDev)

	awake, lit := time.Now().Add(screenWake), false
	var askUntil, stockUntil, nextPoll time.Time
	var info readings
	var shown frame // what the panel is showing now

	// The screen starts before the modem is open, so a reading taken without it
	// is retried soon rather than left up for a whole interval.
	refresh := func() {
		info = poll(info)
		if atModem.Load() == nil {
			nextPoll = time.Now().Add(2 * time.Second)
		} else {
			nextPoll = time.Now().Add(pollEvery)
		}
	}

	for {
		select {
		case reply := <-frameReq:
			if time.Now().After(nextPoll) {
				refresh()
			}
			// Drawing into the page that is not on screen leaves the panel alone.
			buf := s.back()
			s.draw(buf, icons, logo, info, newFrame(info, time.Now().Before(askUntil)))
			reply <- s.toImage(buf)
			shown = frame{} // that page no longer holds the frame we flipped
			continue
		case code := <-press:
			now := time.Now()
			switch {
			case stock:
				stockUntil = now.Add(stockIdle) // the stock UI is driving, keep it
			case code == keyMenu:
				if oled > 0 {
					syscall.Kill(oled, syscall.SIGCONT)
				}
				stock, askUntil = true, time.Time{}
				stockUntil = now.Add(stockIdle)
				log.Printf("[screen] stock UI has the panel")
			case code == keyPower && now.Before(askUntil):
				toggleGateway("power button")
				askUntil = time.Time{}
			case code == keyPower && now.Before(awake):
				askUntil = now.Add(askWindow) // lit already, so this press is the question
			}
			awake = now.Add(screenWake)
		case <-time.After(screenFrame):
		}

		if stock {
			if time.Now().After(stockUntil) {
				if oled > 0 {
					syscall.Kill(oled, syscall.SIGSTOP)
				}
				stock = false
				shown = frame{}     // the stock UI drew over both pages
				awake = time.Time{} // it went idle in stock mode, so stay dark
				log.Printf("[screen] panel back to the gateway")
			}
			continue
		}

		if time.Now().After(awake) {
			if lit {
				setBacklight(0)
				lit = false
			}
			continue // dark: nothing to draw for, and nothing to ask the modem
		}
		if !lit {
			setBacklight(brightness)
			lit = true
		}

		if time.Now().After(nextPoll) {
			refresh()
		}
		next := newFrame(info, time.Now().Before(askUntil))
		if next == shown {
			continue // the panel already shows this
		}
		s.draw(s.back(), icons, logo, info, next)
		if err := s.flip(); err != nil {
			log.Printf("[screen] %v, giving up", err)
			return
		}
		shown = next
	}
}
