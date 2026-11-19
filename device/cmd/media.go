package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
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
		OnState: func(st mediaplayer.State) { go mediaReport(); announcingSoon(st == mediaplayer.Announcing) },
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

// buttonPaused is which music a press last paused ("media": Home
// Assistant's media player, "sendspin": the Sendspin group), for the next
// press to resume; "" when none.
var buttonPaused struct {
	sync.Mutex
	source string
	// resumed and resumedAt: what a press last resumed, and when. Until its
	// music is heard again (a Sendspin stream takes a second or two to
	// restart) a press pauses it again rather than starting a call.
	resumed   string
	resumedAt time.Time
}

// resumeSettle is how long a resumed source counts as playing before its
// music is heard: a Sendspin group's stream starts a second or so after the
// command, and its player buffers 2-3 s more before the first of it sounds.
const resumeSettle = 8 * time.Second

// musicHeardLevel is the music plane's RMS at the speaker above which music
// is heard (PcmSpeaker.MusicLevel, measured as it is written): the stream
// starting is not yet music sounding.
const musicHeardLevel = 1e-3

// musicPress is the action button's music step, the Voice PE's (its
// center button, firmware-voice-incubator voice-pe.yaml): music playing
// here is paused, HA's media player or the whole Sendspin group (Music
// Assistant pauses the group); with none playing, a press resumes what a
// press paused. False when it did neither: the press starts a call.
func musicPress() bool {
	buttonPaused.Lock()
	defer buttonPaused.Unlock()
	mp := mediaPlayer
	ss := sendspinPlayer()
	settling := time.Since(buttonPaused.resumedAt) < resumeSettle
	switch {
	case settling && buttonPaused.resumed == "media" && mp != nil && mp.State() != mediaplayer.Playing:
		mp.Pause()
		buttonPaused.source, buttonPaused.resumed = "media", ""
		log.Println("[media] the button paused the music again, before it was heard")
		return true
	case settling && buttonPaused.resumed == "sendspin" && ss != nil && !ss.GroupPlaying() && ss.Command("pause"):
		buttonPaused.source, buttonPaused.resumed = "sendspin", ""
		log.Println("[sendspin] the button paused the group again, before it was heard")
		return true
	}
	if mp != nil && mp.State() == mediaplayer.Playing {
		mp.Pause()
		buttonPaused.source = "media"
		log.Println("[media] the button paused the music")
		return true
	}
	if ss != nil && ss.GroupPlaying() && ss.Command("pause") {
		buttonPaused.source = "sendspin"
		log.Println("[sendspin] the button paused the group")
		return true
	}
	was := buttonPaused.source
	buttonPaused.source = ""
	switch {
	case was == "media" && mp != nil && mp.State() == mediaplayer.Paused:
		mp.Resume()
		buttonPaused.resumed, buttonPaused.resumedAt = "media", time.Now()
		log.Println("[media] the button resumed the music")
		go musicRingRepaint() // the waiting ring (off this lock: the paint asks buttonResuming)
		return true
	case was == "sendspin" && ss != nil && ss.Command("play"):
		buttonPaused.resumed, buttonPaused.resumedAt = "sendspin", time.Now()
		log.Println("[sendspin] the button resumed the group")
		go musicRingRepaint()
		return true
	}
	return false
}

// announcingSoon tells the satellite's app when an announcement from Home
// Assistant begins and ends (the Voice PE's on_announcement and on_state):
// the app waits on them when Home Assistant speaks its reply here. Called
// under the player's lock, so it hands each change, in order, to one
// goroutine that passes it on.
var (
	mediaAnnouncing atomic.Bool
	announceCh      = make(chan bool, 16)
	announceOnce    sync.Once
)

func announcingSoon(on bool) {
	if mediaAnnouncing.Swap(on) == on {
		return
	}
	announceOnce.Do(func() {
		go func() {
			for on := range announceCh {
				if incubatorMgr != nil {
					incubatorMgr.Announcing(on)
				}
			}
		}()
	})
	select {
	case announceCh <- on:
	default: // sixteen changes behind: the app's wait times out instead
	}
}

// buttonResuming says whether a press resumed music that is not heard yet:
// the ring shows it waits (resumeWaitSpec), for at most resumeSettle.
func buttonResuming() bool {
	buttonPaused.Lock()
	waiting := buttonPaused.resumed != "" && time.Since(buttonPaused.resumedAt) < resumeSettle
	buttonPaused.Unlock()
	return waiting && !(mediaSpk != nil && mediaSpk.MusicLevel() > musicHeardLevel)
}

// resumeWaitSpec is the idle ring while resumed music is on its way: a slow
// spin in the LED Ring light's colour, at its brightness.
func resumeWaitSpec() server.AnimSpec {
	c, _ := ledRingColour()
	if c == ([3]uint8{}) {
		c = [3]uint8{160, 160, 160}
	}
	return server.AnimSpec{Pattern: "spin", Colors: [][3]uint8{c, {c[0] / 4, c[1] / 4, c[2] / 4}}, PeriodMs: 120}
}
