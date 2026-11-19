package main

import (
	"context"
	"errors"
	"log"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/wilbowes/EchoMuse/internal/config"
	"github.com/wilbowes/EchoMuse/internal/dhcpname"
)

// The Echo's DHCP hostname (internal/dhcpname): the controller's
// dhcpHostname, kept in the kept config, so it is sent from boot with or
// without a controller. Applied at boot and on a push that changes it, one
// at a time, off the config path (it may wait up to 30 s for the client).
var dhcpName struct {
	sync.Mutex
	run     sync.Mutex
	applied string
	pending string
}

func applyDHCPHostname() {
	name := config.Get().Snapshot().DhcpHostname
	dhcpName.Lock()
	if name == "" || name == dhcpName.applied || name == dhcpName.pending {
		dhcpName.Unlock()
		return
	}
	dhcpName.pending = name
	dhcpName.Unlock()
	go func() {
		dhcpName.run.Lock()
		defer dhcpName.run.Unlock()
		env := dhcpname.Env{
			Kill: func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) },
			Run: func(path string, args ...string) ([]byte, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				return exec.CommandContext(ctx, path, args...).CombinedOutput()
			},
		}
		err := env.Apply(name)
		dhcpName.Lock()
		if dhcpName.pending == name {
			dhcpName.pending = ""
		}
		if err == nil || errors.Is(err, dhcpname.ErrNotHere) {
			dhcpName.applied = name // nothing more to try for this name
		}
		dhcpName.Unlock()
		switch {
		case err == nil:
			log.Printf("[dhcp] the DHCP client sends the hostname %q", name)
		case errors.Is(err, dhcpname.ErrNotHere):
			log.Printf("[dhcp] hostname %q not sent: no emOS udhcpc to wrap on this Echo", name)
		default:
			log.Printf("[dhcp] hostname %q not sent: %v", name, err)
		}
	}()
}
