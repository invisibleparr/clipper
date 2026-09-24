package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// fmtT renders "m:ss.t", switching to "h:mm:ss.t" past one hour.
// Rounding first keeps 59.96 from printing as the impossible "0:60.0".
func fmtT(t float64) string {
	t = math.Round(t*10) / 10
	m := int(t) / 60
	s := t - float64(m*60)
	if h := m / 60; h > 0 {
		return fmt.Sprintf("%d:%02d:%04.1f", h, m%60, s)
	}
	return fmt.Sprintf("%d:%04.1f", m, s)
}

func (u *ui) updateTimeLabel() {
	u.timeLbl.SetText(fmtT(u.pos) + " / " + fmtT(u.duration))
}

// updateCuts refreshes everything that shows the current cut points.
func (u *ui) updateCuts() {
	if u.duration <= 0 {
		return
	}
	u.clipLbl.SetText(fmt.Sprintf("clip %.1fs", u.end-u.start))
	u.startEntry.SetText(fmtT(u.start))
	u.endEntry.SetText(fmtT(u.end))
}

// setCut applies a typed timestamp to the start (which=1) or end (which=2).
func (u *ui) setCut(which int, text string) {
	if u.duration <= 0 {
		return
	}
	v, ok := parseTS(text)
	if !ok {
		u.status.SetText("invalid time: " + text)
		u.updateCuts() // revert entry text
		return
	}
	if which == 1 {
		v = clampF(v, 0, u.end-minClipLen)
		u.start, u.trim.start = v, v
	} else {
		v = clampF(v, u.start+minClipLen, u.duration)
		u.end, u.trim.end = v, v
	}
	u.trim.Refresh()
	u.updateCuts()
	u.scrub(v, true) // show the frame at the new cut point
	u.estimate()
}

// parseTS accepts seconds "9.3", minutes "1:02.5" and hours "1:02:03";
// comma decimals allowed, only the last part may be fractional.
func parseTS(s string) (float64, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, false
	}
	total := 0.0
	for i, p := range parts {
		p = strings.TrimSpace(p)
		var v float64
		if i < len(parts)-1 {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 {
				return 0, false
			}
			v = float64(n)
		} else {
			f, err := strconv.ParseFloat(p, 64)
			if err != nil || f < 0 {
				return 0, false
			}
			v = f
		}
		switch len(parts) - i { // last part is seconds, then minutes, then hours
		case 3:
			total += v * 3600
		case 2:
			total += v * 60
		default:
			total += v
		}
	}
	return total, true
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// fmtSize renders a byte count like phone storage screens (decimal units).
func fmtSize(b int64) string {
	switch {
	case b < 1000:
		return fmt.Sprintf("%d B", b)
	case b < 1_000_000:
		return fmt.Sprintf("%.0f KB", float64(b)/1e3)
	case b < 1_000_000_000:
		return fmt.Sprintf("%.1f MB", float64(b)/1e6)
	}
	return fmt.Sprintf("%.2f GB", float64(b)/1e9)
}
