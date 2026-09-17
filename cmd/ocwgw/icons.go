// The stock UI keeps its status icons in an XML file on the modem: each one is a
// position, a size, and a run of frames, one per state. The frames are 1 bit per
// pixel, packed MSB first, row-padded to whole bytes, and a zero bit is ink.
//
// We read that file at startup and draw the same icons in the same places, so
// the top of our screen looks like the top of the one Huawei ships. Nothing is
// copied into this repo: a modem whose firmware puts the file elsewhere, or
// whose icons differ, just gets what its own file says. If it is not there at
// all we fall back to drawing the bars ourselves.

package main

import (
	"encoding/hex"
	"encoding/xml"
	"log"
	"os"
	"strings"
)

const iconXMLPath = "/app/webroot/WebApp/common/config/oled/icon.xml"

type icon struct {
	Sx     int      `xml:"sx"`
	Sy     int      `xml:"sy"`
	Width  int      `xml:"width"`
	Height int      `xml:"height"`
	Frames []string `xml:"bitmap>frame"`

	bits [][]byte
}

type iconSet struct {
	Signal  icon `xml:"signal"`
	Mode    icon `xml:"mode_type"`
	Wlan    icon `xml:"wlan"`
	Users   icon `xml:"wifi_users"`
	Message icon `xml:"message"`
	Battery icon `xml:"battery_level"`
}

func loadIcons(path string) *iconSet {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[screen] stock icons: %v, drawing our own", err)
		return nil
	}
	var set iconSet
	if err := xml.Unmarshal(b, &set); err != nil {
		log.Printf("[screen] stock icons: %v, drawing our own", err)
		return nil
	}
	for _, ic := range []*icon{&set.Signal, &set.Mode, &set.Wlan, &set.Users, &set.Message, &set.Battery} {
		ic.decode()
	}
	if len(set.Signal.bits) == 0 || len(set.Battery.bits) == 0 {
		log.Printf("[screen] stock icons: %s has no signal or battery frames", path)
		return nil
	}
	return &set
}

// decode turns the "0xFF,0x80," frame text into packed rows.
func (ic *icon) decode() {
	want := (ic.Width + 7) / 8 * ic.Height
	for _, f := range ic.Frames {
		clean := strings.NewReplacer("0x", "", ",", "", " ", "", "\n", "", "\r", "", "\t", "").Replace(f)
		raw, err := hex.DecodeString(clean)
		if err != nil || len(raw) < want {
			ic.bits = nil
			return
		}
		ic.bits = append(ic.bits, raw)
	}
}

// icon paints frame n at the icon's own position. Out-of-range frames clamp, so
// a firmware with fewer frames than we expect still draws something.
func (s *screen) icon(buf []byte, ic icon, n int, c uint16) {
	if len(ic.bits) == 0 {
		return
	}
	if n < 0 {
		n = 0
	}
	if n >= len(ic.bits) {
		n = len(ic.bits) - 1
	}
	stride := (ic.Width + 7) / 8
	for y := 0; y < ic.Height; y++ {
		for x := 0; x < ic.Width; x++ {
			if ic.bits[n][y*stride+x/8]>>(7-uint(x%8))&1 == 0 { // 0 is ink
				s.set(buf, ic.Sx+x, ic.Sy+y, c)
			}
		}
	}
}
