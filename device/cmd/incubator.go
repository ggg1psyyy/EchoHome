package main

import (
	"github.com/wilbowes/EchoMuse/internal/client"
	"github.com/wilbowes/EchoMuse/internal/firewall"
	"github.com/wilbowes/EchoMuse/internal/incubator"
	"github.com/wilbowes/EchoMuse/pkg/board"
)

// The Voice Incubator satellite (internal/incubator), run when the
// controller's incubator_satellite message says so. Like the Sendspin player
// it stops when the controller link goes (EchoMuse is one system: the
// controller is also what advertises the Echo on the app's network) and
// starts again on the next message, which the controller sends on connect.
// The decisions are incubator.Manager's, tested there.

type echoFirewall struct{}

func (echoFirewall) Open(port int) error { return firewall.Open(port) }
func (echoFirewall) Close(port int)      { firewall.Close(port) }

var incubatorMgr *incubator.Manager

func setupIncubator(cc *client.ControlClient) {
	incubatorMgr = &incubator.Manager{
		Firewall: echoFirewall{},
		Send:     func(st incubator.State) { cc.SendIncubatorState(st) },
		Model:    "echomuse-" + board.IDOf(board.Current()),
		Firmware: client.Version,
	}
	cc.OnIncubatorSatellite(func(m client.IncubatorSatellite) {
		incubatorMgr.Apply(incubator.Settings{Enabled: m.Enabled, Name: m.Name, MAC: m.MAC, Port: m.Port})
	})
}

// incubatorLinkDown stops the satellite with the controller link.
func incubatorLinkDown() {
	if incubatorMgr != nil {
		incubatorMgr.LinkDown()
	}
}
