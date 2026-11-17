package client

import "log"

// The microphone for the Voice Incubator satellite the Echo runs itself
// (internal/incubator): the wake stream's 80 ms frames, processed as the
// controller's are (beam, gain, AEC; no gate, no AGC), handed to the
// satellite for as long as it is open: while a call listens, and while the
// app's wake word models are loaded (they run on the Echo, and nothing
// leaves it until one fires). The satellite closes it at mute, before
// StopMic, so the stream it would otherwise keep (below) stops with it.
//
// The satellite needs the microphone whether or not the controller wants a wake
// stream (with the wake word off it keeps the stream down), so with no
// stream running it starts one of its own, which sends nothing to the
// controller (callOnly). A controller mic_start replaces that with its own
// stream, which feeds the call too; a mic_stop during a call leaves the
// stream to the call; a lost data link does not end it. A bounded turn
// (lock_mic) takes the microphone from a call: its frames are gated and
// gained for the controller's STT, and not tapped. Under incubator_echo the
// controller runs no turns on the Echo.

// OpenCallMic starts delivering frames to deliver, which must not block
// (the microphone path calls it).
func (d *DataClient) OpenCallMic(deliver func([]byte)) {
	d.callTap.Store(&deliver)
	d.micMu.Lock()
	defer d.micMu.Unlock()
	if !d.micActive {
		d.startCallStreamLocked()
	}
}

// CloseCallMic stops delivery, and the stream if it ran for the call alone.
func (d *DataClient) CloseCallMic() {
	d.callTap.Store(nil)
	d.micMu.Lock()
	defer d.micMu.Unlock()
	if d.micActive && d.callOnly.Load() {
		d.stopCallStreamLocked()
		log.Println("[data] call microphone stopped")
	}
}

// startCallStreamLocked runs a stream for the call alone. micMu held. It has
// no connection: callOnly keeps it from ever writing.
func (d *DataClient) startCallStreamLocked() {
	if d.mic == nil {
		return
	}
	d.callOnly.Store(true)
	d.micActive = true
	d.micStopCh = make(chan struct{})
	d.micConn = nil
	d.micStreamLock = false
	go d.streamMic(nil, d.micStopCh, false)
	log.Println("[data] call microphone started")
}

func (d *DataClient) stopCallStreamLocked() {
	close(d.micStopCh)
	d.micActive = false
	d.callOnly.Store(false)
}
