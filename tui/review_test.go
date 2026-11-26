//go:build !windows

package main

import (
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

func TestReviewEmulatorBlocksOnReplies(t *testing.T) {
	em := vt.NewEmulator(80, 24)
	done := make(chan struct{})
	go func() {
		_, _ = em.Write([]byte(strings.Repeat("\x1b[c", 10)))
		close(done)
	}()
	select {
	case <-done:
		t.Log("emulator did not block")
	case <-time.After(2 * time.Second):
		t.Log("CONFIRMED: em.Write blocks while nobody reads the replies")
	}
}

func TestReviewC1Control(t *testing.T) {
	got := 0
	em := vt.NewEmulator(80, 24)
	em.RegisterOscHandler(ctlOSC, func(data []byte) bool { got++; return true })
	go func() {
		b := make([]byte, 100)
		for {
			if _, err := em.Read(b); err != nil {
				return
			}
		}
	}()
	_, _ = em.Write([]byte("\x9d7799;split;lr\x07"))
	t.Logf("C1 OSC handled: %d", got)
}

func TestReviewPlaceTiny(t *testing.T) {
	var ps []*pane
	for i := 0; i < 16; i++ {
		ps = append(ps, &pane{id: i})
	}
	for k := range presetNames {
		root := preset(ps, k)
		root.fix(nil)
		for _, sz := range [][2]int{{0, 0}, {1, 1}, {2, 2}, {3, 1}} {
			root.place(uvRect(sz[0], sz[1]))
			for _, p := range ps {
				n := root.find(p)
				if n.rect.Dx() < 0 || n.rect.Dy() < 0 || n.rect.Max.X > sz[0]+1 {
					t.Logf("preset %d size %v: pane %d rect %v", k, sz, p.id, n.rect)
					break
				}
			}
		}
	}
}

func uvRect(w, h int) uv.Rectangle { return uv.Rect(0, 0, w, h) }

func TestReviewDrawTiny(t *testing.T) {
	for _, sz := range [][2]int{{1, 1}, {2, 2}, {3, 2}} {
		var ps []*pane
		for i := 0; i < 16; i++ {
			p := &pane{id: i, em: vt.NewEmulator(1, 1), io: &paneIO{resize: func(int, int) {}}}
			go func() {
				b := make([]byte, 64)
				for {
					if _, err := p.em.Read(b); err != nil {
						return
					}
				}
			}()
			p.em.Write([]byte("ｗｉｄｅ漢字\x1b[31mhello"))
			ps = append(ps, p)
		}
		a := &app{w: sz[0], h: sz[1], focus: ps[0]}
		for k := range presetNames {
			a.root = preset(ps, k)
			a.root.fix(nil)
			a.root.place(a.area())
			scr := uv.NewScreenBuffer(sz[0], sz[1])
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("CONFIRMED panic size %v preset %d: %v", sz, k, r)
					}
				}()
				a.drawNode(scr, a.root)
				for _, p := range ps {
					n := a.root.find(p)
					p.resize(n.rect.Dx(), n.rect.Dy())
				}
			}()
		}
	}
}
