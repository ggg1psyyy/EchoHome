package client

import (
	"sync"
	"time"
)

// fanoutMic is a minimal mic.Subscribable: a background pump broadcasts raw
// 9ch S24_3LE periods to every subscriber until closed, mimicking
// PcmMicrophone's fan-out (including the drop-when-full behaviour).
type fanoutMic struct {
	mu     sync.Mutex
	subs   []chan []byte
	stopCh chan struct{}
}

func newFanoutMic() *fanoutMic {
	m := &fanoutMic{stopCh: make(chan struct{})}
	// One 512-frame period of 9ch S24_3LE (the minimum Process() analyses),
	// non-zero so the beamformer smoothers see real energy.
	raw := make([]byte, 512*9*3)
	for i := range raw {
		raw[i] = byte(i % 251)
	}
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-m.stopCh:
				return
			case <-ticker.C:
				m.mu.Lock()
				for _, ch := range m.subs {
					select {
					case ch <- raw:
					default:
					}
				}
				m.mu.Unlock()
			}
		}
	}()
	return m
}

func (m *fanoutMic) Subscribe() chan []byte {
	ch := make(chan []byte, 32)
	m.mu.Lock()
	m.subs = append(m.subs, ch)
	m.mu.Unlock()
	return ch
}

func (m *fanoutMic) Unsubscribe(ch chan []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, s := range m.subs {
		if s == ch {
			m.subs = append(m.subs[:i], m.subs[i+1:]...)
			close(ch)
			return
		}
	}
}

func (m *fanoutMic) close() { close(m.stopCh) }
