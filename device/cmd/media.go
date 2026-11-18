package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/wilbowes/EchoMuse/internal/bindings/speaker"
	"github.com/wilbowes/EchoMuse/internal/esphome"
	"github.com/wilbowes/EchoMuse/internal/esphome/api"
	"github.com/wilbowes/EchoMuse/internal/mediaplayer"
	"github.com/wilbowes/EchoMuse/internal/server"
)

// The Echo's own media player on its ESPHome device (internal/mediaplayer):
// Home Assistant hands it a URL, for music, a TTS reply played to it, an
// announcement or Music Assistant, and it fetches and plays it, as a Voice
// PE does. The entity is the controller's media player's (key 1, object id
// media_player, no name: HA shows the device's), test-pinned.
//
// HA's ffmpeg proxy delivers WAV at the speaker's format, asked for by
// supported_formats, so nothing is decoded here. Volume is the Echo's own
// (the buttons' grid, PercentToLevel as the controller maps HA's); HA's
// mute silences the speaker without touching the kept volume, so a muted
// Echo never boots silent; any volume change unmutes, as a button press
// would be expected to.

const esphomeMediaPlayerKey = 1 // MEDIA_PLAYER_KEY

// esphomeMediaFeatures are the controller's MEDIA_PLAYER_FEATURES: PAUSE,
// VOLUME_SET, VOLUME_MUTE, PLAY_MEDIA, STOP, PLAY, BROWSE_MEDIA (HA's own
// browser) and MEDIA_ANNOUNCE.
const esphomeMediaFeatures = 1200653

var (
	mediaPlayer *mediaplayer.Player
	mediaEntity *esphome.MediaPlayer
	mediaSrv    *server.Server
	mediaSpk    *speaker.PcmSpeaker
	mediaMuted  atomic.Bool
)

// mediaOut is the speaker as the player sees it.
type mediaOut struct{ *speaker.PcmSpeaker }

func (o mediaOut) PumpVoice(p []byte) error { return o.PumpPeriod(p) }
func (o mediaOut) EndVoice()                { o.EndStream() }
func (o mediaOut) FlushVoice()              { o.Flush() }

// mediaHTTP fetches HA's URLs: no overall timeout (a stream is as long as
// the music), but bounded dialling and headers. Go's own resolver reads
// /etc/resolv.conf, which emOS writes from the DHCP lease; bionic's cannot
// resolve at all there.
var mediaHTTP = &http.Client{Transport: &http.Transport{
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, Resolver: &net.Resolver{PreferGo: true}}).DialContext,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 20 * time.Second,
}}

func mediaFetch(ctx context.Context, u string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := mediaHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// mediaWhere is a URL for the log: scheme and host, never the path (HA's
// proxy paths are one-time ids).
func mediaWhere(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "an unreadable URL"
	}
	return p.Scheme + "://" + p.Host
}

// mediaEntityFor builds the player and its entity.
func mediaEntityFor(s *server.Server, spk *speaker.PcmSpeaker) *esphome.MediaPlayer {
	mediaSrv, mediaSpk = s, spk
	mediaPlayer = &mediaplayer.Player{
		Out:     mediaOut{spk},
		Fetch:   mediaFetch,
		OnState: func(mediaplayer.State) { go mediaReport() },
		Duck:    duckHold,
	}
	wav := func(purpose api.MediaPlayerFormatPurpose) *api.MediaPlayerSupportedFormat {
		return &api.MediaPlayerSupportedFormat{Format: "wav", SampleRate: 48000, NumChannels: 1, Purpose: purpose, SampleBytes: 2}
	}
	mediaEntity = &esphome.MediaPlayer{Desc: &api.ListEntitiesMediaPlayerResponse{ObjectId: "media_player",
		Key: esphomeMediaPlayerKey, Name: "", SupportsPause: true, FeatureFlags: esphomeMediaFeatures,
		SupportedFormats: []*api.MediaPlayerSupportedFormat{
			wav(api.MediaPlayerFormatPurpose_MEDIA_PLAYER_FORMAT_PURPOSE_DEFAULT),
			wav(api.MediaPlayerFormatPurpose_MEDIA_PLAYER_FORMAT_PURPOSE_ANNOUNCEMENT),
		}}}
	mediaEntity.OnCommand = mediaCommand
	mediaReport()
	return mediaEntity
}

// mediaCommand is one of Home Assistant's.
func mediaCommand(c *api.MediaPlayerCommandRequest) {
	p := mediaPlayer
	if c.HasMediaUrl {
		if c.HasAnnouncement && c.Announcement {
			log.Printf("[media] announcement from %s", mediaWhere(c.MediaUrl))
			p.Announce(c.MediaUrl)
		} else if mediaSpk.MusicPlaneBusy() && p.State() == mediaplayer.Idle {
			// The controller's music has the plane; two producers on it
			// would interleave.
			log.Printf("[media] %s refused: the controller is playing music on this Echo", mediaWhere(c.MediaUrl))
		} else {
			log.Printf("[media] playing from %s", mediaWhere(c.MediaUrl))
			p.Play(c.MediaUrl)
		}
	}
	if c.HasVolume {
		mediaSrv.SetVolume(server.PercentToLevel(float64(c.Volume) * 100)) // the change callback reports it
	}
	if c.HasCommand {
		switch c.Command {
		case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_PLAY:
			p.Resume()
		case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_PAUSE:
			p.Pause()
		case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_TOGGLE:
			if p.State() == mediaplayer.Paused {
				p.Resume()
			} else {
				p.Pause()
			}
		case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_STOP:
			p.Stop()
		case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_MUTE:
			mediaMuted.Store(true)
			mediaSpk.SetVolume(0) // the speaker alone: the kept volume stays
		case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_UNMUTE:
			mediaMuted.Store(false)
			mediaSpk.SetVolume(mediaSrv.VolumeLevel())
		case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_VOLUME_UP:
			mediaSrv.VolumeStepUp()
		case api.MediaPlayerCommand_MEDIA_PLAYER_COMMAND_VOLUME_DOWN:
			mediaSrv.VolumeStepDown()
		}
	}
	mediaReport()
}

// mediaVolumeChanged follows the Echo's volume, however it changed; a
// change applies the level to the speaker, so a mute is over.
func mediaVolumeChanged(int) {
	if mediaEntity == nil {
		return
	}
	mediaMuted.Store(false)
	mediaReport()
}

// mediaReport sends HA the player's state, volume and mute.
func mediaReport() {
	if mediaEntity == nil || mediaPlayer == nil {
		return
	}
	st := map[mediaplayer.State]api.MediaPlayerState{
		mediaplayer.Idle:       api.MediaPlayerState_MEDIA_PLAYER_STATE_IDLE,
		mediaplayer.Playing:    api.MediaPlayerState_MEDIA_PLAYER_STATE_PLAYING,
		mediaplayer.Paused:     api.MediaPlayerState_MEDIA_PLAYER_STATE_PAUSED,
		mediaplayer.Announcing: api.MediaPlayerState_MEDIA_PLAYER_STATE_ANNOUNCING,
	}[mediaPlayer.State()]
	vol := float32(server.LevelToPercent(mediaSrv.VolumeLevel()) / 100)
	mediaEntity.Set(st, vol, mediaMuted.Load())
}

// mediaStreamEnded takes the speaker's voice end report for an
// announcement; false leaves it to the others.
func mediaStreamEnded() bool { return mediaPlayer != nil && mediaPlayer.StreamEnded() }
