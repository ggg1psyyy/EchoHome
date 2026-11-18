package mic

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// An ALSA open blocks while another process holds the substream, and a
// capture open that blocked was seen never to come back, even once the
// holder had gone (bench, 00LQ, 2026-11-18: a server killed and restarted at
// once; the new one's capture never delivered, the satellite heard nothing
// and nothing was logged). So, as the speaker does (waitForFreePcm), the
// capture substream is waited on before the open, and the holder named.

const captureFreeTimeout = 10 * time.Second

func captureStatusPath(card, device int) string {
	return fmt.Sprintf("/proc/asound/card%d/pcm%dc/sub0/status", card, device)
}

// captureFree is the speaker's pcmFree: "closed" is free, a status that
// leads with "state:" is held, anything else counts as free (failing open,
// as before this guard).
func captureFree(status string) bool {
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(status), "\n", 2)[0])
	if strings.EqualFold(first, "closed") {
		return true
	}
	key, _, found := strings.Cut(first, ":")
	return !(found && strings.EqualFold(strings.TrimSpace(key), "state"))
}

func captureOwner(status string) int {
	for _, line := range strings.Split(status, "\n") {
		key, val, found := strings.Cut(line, ":")
		if found && strings.TrimSpace(key) == "owner_pid" {
			if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
				return n
			}
		}
	}
	return 0
}

// waitForFreeCapture blocks until the capture substream is released or the
// timeout passes, then lets the open go ahead either way.
func waitForFreeCapture(card, device int, timeout time.Duration) {
	path := captureStatusPath(card, device)
	b, err := os.ReadFile(path)
	if err != nil || captureFree(string(b)) {
		return
	}
	log.Printf("[mic] %s held by pid %d — waiting up to %s", path, captureOwner(string(b)), timeout)
	start := time.Now()
	for time.Since(start) < timeout {
		time.Sleep(200 * time.Millisecond)
		if b, err := os.ReadFile(path); err != nil || captureFree(string(b)) {
			log.Printf("[mic] capture released after %s", time.Since(start).Round(time.Millisecond))
			return
		}
	}
	b, _ = os.ReadFile(path)
	log.Printf("[mic] capture STILL held by pid %d after %s — opening anyway, this may block", captureOwner(string(b)), timeout)
}
