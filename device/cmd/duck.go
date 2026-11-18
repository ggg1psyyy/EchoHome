package main

import (
	"sync"

	"github.com/wilbowes/EchoMuse/internal/bindings/speaker"
	"github.com/wilbowes/EchoMuse/internal/config"
)

// The Echo's own ducks of the music (the Voice Incubator satellite's turns,
// the media player's announcements) are counted, so one ending never lifts
// another's: the music comes back up only when none holds it. The
// controller's ducks arrive on the data link and are its own.
var duckCount struct {
	sync.Mutex
	n   int
	spk *speaker.PcmSpeaker
}

func setupDuck(spk *speaker.PcmSpeaker) {
	duckCount.Lock()
	duckCount.spk = spk
	duckCount.Unlock()
}

// duckHold takes (true) or gives back (false) one hold on the duck.
func duckHold(on bool) {
	duckCount.Lock()
	defer duckCount.Unlock()
	if duckCount.spk == nil {
		return
	}
	switch {
	case on:
		duckCount.n++
		if duckCount.n == 1 {
			duckCount.spk.SetDuck(config.Get().DuckDb)
		}
	case duckCount.n > 0:
		duckCount.n--
		if duckCount.n == 0 {
			duckCount.spk.SetDuck(0)
		}
	}
}
