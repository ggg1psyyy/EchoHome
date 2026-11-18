package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/wilbowes/EchoMuse/internal/esphome"
	"github.com/wilbowes/EchoMuse/internal/esphome/api"
	"github.com/wilbowes/EchoMuse/internal/mediaplayer"
)

// A stand-in speaker: plays a period every 2 ms (20x real time) and logs
// what each plane played.
type fastOut struct {
	mu           sync.Mutex
	music, voice [][]byte
	voiceEnd     bool
	p            *mediaplayer.Player
}

func (f *fastOut) PumpMusic(b []byte) error {
	f.mu.Lock()
	f.music = append(f.music, b)
	f.mu.Unlock()
	return nil
}
func (f *fastOut) EndMusicStream() {}
func (f *fastOut) FlushMusic()     { f.mu.Lock(); f.music = nil; f.mu.Unlock() }
func (f *fastOut) MusicLead() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return time.Duration(len(f.music)) * 2048 * time.Second / 48000
}
func (f *fastOut) PumpVoice(b []byte) error {
	f.mu.Lock()
	f.voice = append(f.voice, b)
	f.mu.Unlock()
	return nil
}
func (f *fastOut) EndVoice()                     { f.mu.Lock(); f.voiceEnd = true; f.mu.Unlock() }
func (f *fastOut) FlushVoice()                   { f.mu.Lock(); f.voice = nil; f.mu.Unlock() }
func (f *fastOut) VoiceSoundingUntil() time.Time { return time.Now() }

func (f *fastOut) run() {
	for range time.Tick(2 * time.Millisecond) {
		f.mu.Lock()
		if len(f.music) > 0 {
			f.music = f.music[1:]
		}
		ended := false
		if len(f.voice) > 0 {
			f.voice = f.voice[1:]
			ended = len(f.voice) == 0 && f.voiceEnd
		}
		if ended {
			f.voiceEnd = false
		}
		f.mu.Unlock()
		if ended {
			f.p.StreamEnded()
		}
	}
}

func fetch(ctx context.Context, url string) (io.ReadCloser, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func mediaPlayerEntity() *esphome.MediaPlayer {
	out := &fastOut{}
	var mp *esphome.MediaPlayer
	var vol float32 = 0.5
	var muted bool
	report := func(st mediaplayer.State) {
		mp.Set(api.MediaPlayerState(st+1), vol, muted) // Idle=1 .. Announcing=4
	}
	p := &mediaplayer.Player{Out: out, Fetch: fetch, OnState: func(st mediaplayer.State) { go report(st) }}
	out.p = p
	go out.run()
	mp = &esphome.MediaPlayer{Desc: &api.ListEntitiesMediaPlayerResponse{ObjectId: "media_player", Key: 1,
		SupportsPause: true, FeatureFlags: 1200653}}
	mp.OnCommand = func(c *api.MediaPlayerCommandRequest) {
		log.Printf("command: url=%v %q announce=%v volume=%v %v command=%v %v", c.HasMediaUrl, c.MediaUrl,
			c.HasAnnouncement && c.Announcement, c.HasVolume, c.Volume, c.HasCommand, c.Command)
		if c.HasVolume {
			vol = c.Volume
		}
		if c.HasCommand && c.Command == api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_MUTE {
			muted = true
		}
		switch {
		case c.HasMediaUrl && c.HasAnnouncement && c.Announcement:
			p.Announce(c.MediaUrl)
		case c.HasMediaUrl:
			p.Play(c.MediaUrl)
		case c.HasCommand && c.Command == api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_PAUSE:
			p.Pause()
		case c.HasCommand && c.Command == api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_PLAY:
			p.Resume()
		case c.HasCommand && c.Command == api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_STOP:
			p.Stop()
		}
		report(p.State())
	}
	report(mediaplayer.Idle)
	return mp
}

// serveWav serves /song.wav (6 s) and /ann.wav (1 s) on port+1.
func serveWav(port string) {
	n, _ := strconv.Atoi(port)
	mk := func(seconds int) []byte {
		data := make([]byte, seconds*96000)
		b := []byte("RIFF\xff\xff\xff\xffWAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00")
		b = binary.LittleEndian.AppendUint32(b, 48000)
		b = binary.LittleEndian.AppendUint32(b, 96000)
		b = append(b, 2, 0, 16, 0)
		b = append(b, "data\xff\xff\xff\xff"...)
		return append(b, data...)
	}
	files := map[string][]byte{"/song.wav": mk(6), "/ann.wav": mk(1)}
	go http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", n+1), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := files[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
}
