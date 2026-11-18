package client

import "log"

// The microphone for the Voice Incubator satellite the Echo runs itself
// (internal/incubator): 80 ms frames, processed as the old wake stream's
// were (beam, gain, AEC; no gate, no AGC), handed to the satellite for as
// long as it is open: while a call listens, and while the app's wake word
// models are loaded (they run on the Echo, and nothing leaves it until one
// fires). The satellite closes it at mute.
//
// The stream is the satellite's alone since EchoHome: the controller no
// longer streams or starts one, and a lost link does not end it.

// OpenCallMic starts delivering frames to deliver, which must not block
// (the microphone path calls it).
func (d *DataClient) OpenCallMic(deliver func([]byte)) {
	d.callTap.Store(&deliver)
	d.micMu.Lock()
	defer d.micMu.Unlock()
	if d.micActive || d.mic == nil {
		return
	}
	d.micActive = true
	d.micStopCh = make(chan struct{})
	go d.streamMic(d.micStopCh)
	log.Println("[data] call microphone started")
}

// CloseCallMic stops delivery and the stream.
func (d *DataClient) CloseCallMic() {
	d.callTap.Store(nil)
	d.micMu.Lock()
	defer d.micMu.Unlock()
	if !d.micActive {
		return
	}
	close(d.micStopCh)
	d.micActive = false
	log.Println("[data] call microphone stopped")
}
