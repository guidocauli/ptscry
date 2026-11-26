package main

import (
	"image"
	"testing"
	"time"
)

func reviewGUI() *gui {
	return &gui{wake: make(chan struct{}, 1), ctl: make(chan ctlReq, 16)}
}

func wire(g *gui, t *tab) {
	t.model.onCtl = func(args []string) {
		select {
		case g.ctl <- ctlReq{t, args}:
		default:
		}
	}
}

// A pane on a hostile server moves the keyboard focus away from the pane the
// person is typing in, to itself.
func TestReviewFocusSteal(t *testing.T) {
	for _, seq := range []string{"\x1b]7799;focus;right\x07", "\x1b]7799;focus;right\x1b\\", "\x9d7799;focus;right\x07", "\x9d7799;focus;right\x9c"} {
		g := reviewGUI()
		good, evil := mkTab(g, "good"), mkTab(g, "evil")
		evil.profile = "evil"
		wire(g, good)
		wire(g, evil)
		g.tabs = []*tab{good}
		g.insertPane(good, good, evil, true, 0.5)
		good.focus = good
		good.root.rect = image.Rect(0, 0, 100, 100)
		good.root.a.rect = image.Rect(0, 0, 50, 100)
		good.root.b.rect = image.Rect(50, 0, 100, 100)
		evil.liveAt = time.Now().Add(-time.Minute)
		evil.model.Feed([]byte(seq))
		g.takeControls()
		t.Logf("%q -> focused=%s", seq, good.focused().session)
	}
}

// A background tab moves the focus of the foreground tab.
func TestReviewFocusFromBackgroundTab(t *testing.T) {
	g := reviewGUI()
	a1, a2, bg := mkTab(g, "a1"), mkTab(g, "a2"), mkTab(g, "bg")
	wire(g, bg)
	g.tabs = []*tab{a1, bg}
	g.active = 0
	g.insertPane(a1, a1, a2, true, 0.5)
	a1.focus = a1
	a1.root.rect = image.Rect(0, 0, 100, 100)
	a1.root.a.rect = image.Rect(0, 0, 50, 100)
	a1.root.b.rect = image.Rect(50, 0, 100, 100)
	bg.liveAt = time.Now().Add(-time.Minute)
	bg.model.Feed([]byte("\x1b]7799;focus;right\x07"))
	g.takeControls()
	if a1.focused() == a2 {
		t.Errorf("CONFIRMED: output of a background tab moved the focus of the active tab to %s", a1.focused().session)
	}
}

// "session;NAME" changes the session a tab reattaches to, from any output,
// with no grace period after attach.
func TestReviewSessionSpoof(t *testing.T) {
	g := reviewGUI()
	a := mkTab(g, "work")
	wire(g, a)
	g.tabs = []*tab{a}
	a.liveAt = time.Now() // just attached: replay in progress
	a.model.Feed([]byte("\x1b]7799;session;other\x07"))
	// onControl calls saveWorkspace -> disk; only look at the request
	select {
	case r := <-g.ctl:
		t.Logf("request accepted right after attach: %v", r.args)
	default:
		t.Log("no request")
	}
}

// saveWorkspace (called by pollCwds on its own goroutine) walks the pane tree
// without tabMu while the UI goroutine changes it.
