package main

import (
	"log"
	"sync"
	"time"

	"github.com/wilbowes/EchoMuse/internal/esphome"
	"github.com/wilbowes/EchoMuse/internal/esphome/api"
	"github.com/wilbowes/EchoMuse/internal/taps"
	pkgbuttons "github.com/wilbowes/EchoMuse/pkg/buttons"
)

// Double and triple taps of the action button, counted on the Echo
// (internal/taps), as Home Assistant events on the Echo's own device's
// Action Button. Opt-in ("Multi-tap events", kept on the Echo): to tell a
// single tap from a double, a single is decided only after the window, so
// the call it starts starts that much later. On, the action button's
// events are held back and counted: one tap goes on as before (press and
// release, to the controller or the standalone satellite) and is also the
// "single" event; two and three are "double" and "triple" and go only to
// HA; a hold goes on at once (its "long" event, and to whoever takes it),
// after any taps before it. The pairing hold is decided before any of this.

const (
	esphomeMultiTapKey = 26
	localMultiTap      = "multiTap" // local: the controller has no such key
	multiTapWindow     = 400 * time.Millisecond
)

var multiTap struct {
	sync.Mutex
	on       bool
	counter  *taps.Counter
	down, up pkgbuttons.ButtonClickEvent
	dispatch func(pkgbuttons.ButtonClickEvent)
	entity   *esphome.Switch
}

// setMultiTapDispatch gives the action button's usual path.
func setMultiTapDispatch(dispatch func(pkgbuttons.ButtonClickEvent)) {
	multiTap.Lock()
	multiTap.dispatch = dispatch
	multiTap.Unlock()
}

// multiTapEntity is the switch, from the kept setting (after setupOwned).
func multiTapEntity() *esphome.Switch {
	multiTap.Lock()
	defer multiTap.Unlock()
	multiTap.counter = &taps.Counter{Window: multiTapWindow, Emit: multiTapEmit}
	if ownedSettings != nil {
		multiTap.on, _ = ownedSettings.Value(localMultiTap)
	}
	w := &esphome.Switch{Desc: &api.ListEntitiesSwitchResponse{ObjectId: "multi_tap", Key: esphomeMultiTapKey,
		Name: "Multi-tap events", Icon: "mdi:gesture-double-tap",
		EntityCategory: api.EntityCategory_ENTITY_CATEGORY_CONFIG}}
	w.OnCommand = func(on bool) {
		if ownedSettings != nil {
			if err := ownedSettings.SetLocal(localMultiTap, on); err != nil {
				log.Printf("[esphome] multi-tap setting not kept: %v", err)
				return
			}
		}
		multiTap.Lock()
		multiTap.on = on
		multiTap.Unlock()
		w.Set(on)
		log.Printf("[esphome] multi-tap events %v", on)
	}
	w.Set(multiTap.on)
	multiTap.entity = w
	return w
}

// multiTapTake takes an action button event while counting is on; false
// leaves it to the usual path.
func multiTapTake(e pkgbuttons.ButtonClickEvent) bool {
	multiTap.Lock()
	if !multiTap.on || multiTap.counter == nil || multiTap.dispatch == nil || e.ClickType != pkgbuttons.DotClick {
		multiTap.Unlock()
		return false
	}
	c, dispatch := multiTap.counter, multiTap.dispatch
	if e.Down {
		multiTap.down = e
		multiTap.Unlock()
		c.Press()
		return true
	}
	down := multiTap.down
	if int(e.HeldMs) >= incubatorHoldMs {
		multiTap.Unlock()
		c.Hold() // the taps before it first
		dispatch(down)
		dispatch(e)
		return true
	}
	multiTap.up = e
	multiTap.Unlock()
	c.Tap()
	return true
}

func multiTapEmit(n int) {
	multiTap.Lock()
	down, up, dispatch := multiTap.down, multiTap.up, multiTap.dispatch
	multiTap.Unlock()
	switch {
	case n == 1:
		esphomeFire("single")
		dispatch(down)
		dispatch(up)
	case n == 2:
		esphomeFire("double")
	default:
		esphomeFire("triple")
	}
	log.Printf("[cmd] action button: %d tap(s)", n)
}

// esphomeFire sends an Action Button event, if the Echo's server runs.
func esphomeFire(kind string) {
	e := esphomeOnEcho
	if e == nil {
		return
	}
	e.mu.Lock()
	srv := e.srv
	e.mu.Unlock()
	if srv != nil {
		srv.Fire(esphomeEventKey, kind)
	}
}
