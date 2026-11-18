package bluetooth

// One owner of the HCI transport: a single read pump, which sorts packets:
//   - a Command Complete/Status for the command in flight goes to its caller;
//   - advertising reports go to adv, and are DROPPED under backlog — the next
//     one is 100ms away;
//   - everything else is dropped. It went to the connection manager for Home
//     Assistant's active proxy (#656), removed in EchoHome: the Echo's own
//     ESPHome proxy is passive.

import (
	"fmt"
	"io"
	"sync"
	"time"
)

type hciHost struct {
	rw io.ReadWriteCloser

	wmu   sync.Mutex // one packet per write
	cmdMu sync.Mutex // one command in flight

	pendMu sync.Mutex
	pendOp uint16
	pendCh chan commandComplete

	adv    chan []byte   // LE advertising reports
	active chan struct{} // poked on any inbound packet, for the watchdog

	dead chan struct{} // closed when the pump exits
	err  error         // why; set before dead closes
}

func newHCIHost(rw io.ReadWriteCloser) *hciHost {
	h := &hciHost{
		rw:     rw,
		adv:    make(chan []byte, 64),
		active: make(chan struct{}, 1),
		dead:   make(chan struct{}),
	}
	go h.pump()
	return h
}

func (h *hciHost) pump() {
	var parser h4Parser
	buf := make([]byte, 2048)
	for {
		n, err := h.rw.Read(buf)
		if err != nil {
			h.err = err
			close(h.dead)
			return
		}
		for _, pkt := range parser.Feed(buf[:n]) {
			select {
			case h.active <- struct{}{}:
			default:
			}
			h.route(pkt)
		}
	}
}

func (h *hciHost) route(pkt []byte) {
	if cc, ok := parseCommandComplete(pkt); ok {
		h.pendMu.Lock()
		ch := h.pendCh
		match := ch != nil && cc.opcode == h.pendOp
		if match {
			h.pendCh = nil
		}
		h.pendMu.Unlock()
		if match {
			ch <- cc // buffered; never blocks the pump
		}
		return
	}
	if len(pkt) >= 4 && pkt[0] == h4TypeEvent && pkt[1] == evtLEMeta && pkt[3] == leSubeventAdvReport {
		select {
		case h.adv <- pkt:
		default:
		}
		return
	}
}

// Cmd sends one command and waits for its Command Complete or Command Status.
// A non-zero status is returned as an error, with the result alongside.
func (h *hciHost) Cmd(opcode uint16, params []byte) (commandComplete, error) {
	h.cmdMu.Lock()
	defer h.cmdMu.Unlock()
	ch := make(chan commandComplete, 1)
	h.pendMu.Lock()
	h.pendOp, h.pendCh = opcode, ch
	h.pendMu.Unlock()
	forget := func() {
		h.pendMu.Lock()
		h.pendCh = nil
		h.pendMu.Unlock()
	}
	if err := h.write(buildCommand(opcode, params)); err != nil {
		forget()
		return commandComplete{}, fmt.Errorf("write cmd %04x: %w", opcode, err)
	}
	timeout := time.NewTimer(cmdTimeout)
	defer timeout.Stop()
	select {
	case cc := <-ch:
		if cc.status != 0 {
			return cc, fmt.Errorf("cmd %04x status 0x%02x", opcode, cc.status)
		}
		return cc, nil
	case <-h.dead:
		return commandComplete{}, fmt.Errorf("read during cmd %04x: %w", opcode, h.err)
	case <-timeout.C:
		forget()
		return commandComplete{}, fmt.Errorf("cmd %04x timeout", opcode)
	}
}

func (h *hciHost) write(pkt []byte) error {
	h.wmu.Lock()
	defer h.wmu.Unlock()
	_, err := h.rw.Write(pkt)
	return err
}
