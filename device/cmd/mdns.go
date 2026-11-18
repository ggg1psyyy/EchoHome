package main

import (
	"log"
	"net"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

// announce keeps one mDNS service on the network until the returned func
// withdraws it: instance on service, at host (never the emOS hostname:
// Echoes share it, and two answering for one name would send a client to
// the wrong Echo), on the Echo's IPv4 addresses. At boot (a service can
// start before WiFi has an address) and whenever the addresses or the TXT
// records change, the record follows: no address yet is retried, a change
// is re-announced. txt is read on every tick. emOS admits the queries
// (sport 5353, init.c firewall()).
func announce(tag, instance, service, host string, port int, txt func() []string) func() {
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var srv *zeroconf.Server
		var at string
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			ips := localIPv4s()
			records := txt()
			if now := strings.Join(ips, ",") + "|" + strings.Join(records, "\x00"); now != at {
				if srv != nil {
					srv.Shutdown()
					srv = nil
				}
				at = now
				if len(ips) > 0 {
					var err error
					if srv, err = zeroconf.RegisterProxy(instance, service, "local.", port, host, ips, records, nil); err != nil {
						log.Printf("[%s] mDNS announcement failed: %v", tag, err)
						at = "" // try again on the next tick
					} else {
						log.Printf("[%s] announced %s.%s at %s", tag, instance, service, strings.Join(ips, ","))
					}
				}
			}
			select {
			case <-quit:
				if srv != nil {
					srv.Shutdown()
				}
				return
			case <-t.C:
			}
		}
	}()
	return func() { close(quit); <-done }
}

// localIPv4s are the Echo's own IPv4 addresses on interfaces that are up.
func localIPv4s() []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				out = append(out, n.IP.String())
			}
		}
	}
	return out
}
