package diskwriter

import (
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/at-wat/ebml-go/webm"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3/pkg/media/samplebuilder"

	"github.com/jech/galene/conn"
	"github.com/jech/galene/group"
)

var Directory string

type Client struct {
	group *group.Group
	id    string

	mu     sync.Mutex
	down   map[string]*diskConn
	closed bool
}

func newId() string {
	b := make([]byte, 16)
	crand.Read(b)
	return hex.EncodeToString(b)
}

func New(g *group.Group) *Client {
	return &Client{group: g, id: newId()}
}

func (client *Client) Group() *group.Group {
	return client.group
}

func (client *Client) Id() string {
	return client.id
}

func (client *Client) Username() string {
	return "RECORDING"
}

func (client *Client) Challenge(group string, cred group.ClientCredentials) bool {
	return true
}

func (client *Client) OverridePermissions(g *group.Group) bool {
	return true
}

func (client *Client) SetPermissions(perms group.ClientPermissions) {
	return
}

func (client *Client) PushClient(id, username string, add bool) error {
	return nil
}

func (client *Client) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()

	for _, down := range client.down {
		down.Close()
	}
	client.down = nil
	client.closed = true
	return nil
}

func (client *Client) Kick(id, user, message string) error {
	err := client.Close()
	group.DelClient(client)
	return err
}

func (client *Client) PushConn(g *group.Group, id string, up conn.Up, tracks []conn.UpTrack, label string) error {
	if client.group != g {
		return nil
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	if client.closed {
		return errors.New("disk client is closed")
	}

	old := client.down[id]
	if old != nil {
		old.Close()
		delete(client.down, id)
	}

	if up == nil {
		return nil
	}

	directory := filepath.Join(Directory, client.group.Name())
	err := os.MkdirAll(directory, 0700)
	if err != nil {
		g.WallOps("Write to disk: " + err.Error())
		return err
	}

	if client.down == nil {
		client.down = make(map[string]*diskConn)
	}

	down, err := newDiskConn(client, directory, label, up, tracks)
	if err != nil {
		g.WallOps("Write to disk: " + err.Error())
		return err
	}

	client.down[up.Id()] = down
	return nil
}

type diskConn struct {
	client    *Client
	directory string
	label     string
	hasVideo  bool

	mu            sync.Mutex
	file          *os.File
	remote        conn.Up
	tracks        []*diskTrack
	width, height uint32
	lastWarning   time.Time
}

// called locked
func (conn *diskConn) warn(message string) {
	now := time.Now()
	if now.Sub(conn.lastWarning) < 10*time.Second {
		return
	}
	log.Println(message)
	conn.client.group.WallOps(message)
	conn.lastWarning = now
}

// called locked
func (conn *diskConn) reopen() error {
	for _, t := range conn.tracks {
		if t.writer != nil {
			t.writer.Close()
			t.writer = nil
		}
	}
	conn.file = nil

	file, err := openDiskFile(conn.directory, conn.label)
	if err != nil {
		return err
	}

	conn.file = file
	return nil
}

func (conn *diskConn) Close() error {
	conn.remote.DelLocal(conn)

	conn.mu.Lock()
	tracks := make([]*diskTrack, 0, len(conn.tracks))
	for _, t := range conn.tracks {
		if t.writer != nil {
			t.writer.Close()
			t.writer = nil
		}
		tracks = append(tracks, t)
	}
	conn.mu.Unlock()

	for _, t := range tracks {
		t.remote.DelLocal(t)
	}
	return nil
}

func openDiskFile(directory, label string) (*os.File, error) {
	filenameFormat := "2006-01-02T15:04:05.000"
	if runtime.GOOS == "windows" {
		filenameFormat = "2006-01-02T15-04-05-000"
	}

	filename := time.Now().Format(filenameFormat)
	if label != "" {
		filename = filename + "-" + label
	}
	for counter := 0; counter < 100; counter++ {
		var fn string
		if counter == 0 {
			fn = fmt.Sprintf("%v.webm", filename)
		} else {
			fn = fmt.Sprintf("%v-%02d.webm", filename, counter)
		}

		fn = filepath.Join(directory, fn)
		f, err := os.OpenFile(
			fn, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600,
		)
		if err == nil {
			return f, nil
		} else if !os.IsExist(err) {
			return nil, err
		}
	}
	return nil, errors.New("couldn't create file")
}

type diskTrack struct {
	remote conn.UpTrack
	conn   *diskConn

	writer  webm.BlockWriteCloser
	builder *samplebuilder.SampleBuilder

	// bit 32 is a boolean indicating that the origin is valid
	origin uint64

	lastKf uint32
}

func newDiskConn(client *Client, directory, label string, up conn.Up, remoteTracks []conn.UpTrack) (*diskConn, error) {
	conn := diskConn{
		client:    client,
		directory: directory,
		label:     label,
		tracks:    make([]*diskTrack, 0, len(remoteTracks)),
		remote:    up,
	}
	for _, remote := range remoteTracks {
		var builder *samplebuilder.SampleBuilder
		codec := remote.Codec()
		switch strings.ToLower(codec.MimeType) {
		case "audio/opus":
			builder = samplebuilder.New(
				16, &codecs.OpusPacket{}, codec.ClockRate,
				samplebuilder.WithPartitionHeadChecker(
					&codecs.OpusPartitionHeadChecker{},
				),
			)
		case "video/vp8":
			if conn.hasVideo {
				return nil, errors.New("multiple video tracks not supported")
			}
			builder = samplebuilder.New(
				128, &codecs.VP8Packet{}, codec.ClockRate,
				samplebuilder.WithPartitionHeadChecker(
					&codecs.VP8PartitionHeadChecker{},
				),
			)
			conn.hasVideo = true
		default:
			client.group.WallOps(
				"Cannot record codec " + codec.MimeType,
			)
			continue
		}
		track := &diskTrack{
			remote:  remote,
			builder: builder,
			conn:    &conn,
		}
		conn.tracks = append(conn.tracks, track)
		remote.AddLocal(track)
	}

	err := up.AddLocal(&conn)
	if err != nil {
		return nil, err
	}

	return &conn, nil
}

func (t *diskTrack) SetTimeOffset(ntp uint64, rtp uint32) {
}

func (t *diskTrack) SetCname(string) {
}

func clonePacket(packet *rtp.Packet) *rtp.Packet {
	buf, err := packet.Marshal()
	if err != nil {
		return nil
	}
	var p rtp.Packet
	err = p.Unmarshal(buf)
	if err != nil {
		return nil
	}
	return &p
}

func (t *diskTrack) WriteRTP(packet *rtp.Packet) error {
	// since we call initWriter, we take the connection lock for simplicity.
	t.conn.mu.Lock()
	defer t.conn.mu.Unlock()

	if t.builder == nil {
		return nil
	}

	p := clonePacket(packet)
	if p == nil {
		return nil
	}

	kfNeeded := false

	t.builder.Push(p)

	for {
		sample, ts := t.builder.PopWithTimestamp()
		if sample == nil {
			if kfNeeded {
				return conn.ErrKeyframeNeeded
			}
			return nil
		}

		keyframe := true

		codec := t.remote.Codec()
		switch strings.ToLower(codec.MimeType) {
		case "video/vp8":
			if len(sample.Data) < 1 {
				continue
			}
			keyframe = (sample.Data[0]&0x1 == 0)
			if keyframe {
				err := t.initWriter(sample.Data)
				if err != nil {
					t.conn.warn(
						"Write to disk " + err.Error(),
					)
					return err
				}
				t.lastKf = ts
			} else if t.writer != nil {
				// Request a keyframe every 10s
				delta := ts - t.lastKf
				if (delta&0x80000000) == 0 &&
					delta > 10*90000 {
					kfNeeded = true
				}
			}
		default:
			if t.writer == nil {
				if !t.conn.hasVideo {
					err := t.conn.initWriter(0, 0)
					if err != nil {
						t.conn.warn(
							"Write to disk " +
								err.Error(),
						)
						return err
					}
				}
			}
		}

		if t.writer == nil {
			if !keyframe {
				return conn.ErrKeyframeNeeded
			}
			return nil
		}

		if t.origin == 0 {
			t.origin = uint64(ts) | (1 << 32)
		}
		ts -= uint32(t.origin)

		tm := ts / (t.remote.Codec().ClockRate / 1000)
		_, err := t.writer.Write(keyframe, int64(tm), sample.Data)
		if err != nil {
			return err
		}
	}
}

// called locked
func (t *diskTrack) initWriter(data []byte) error {
	codec := t.remote.Codec()
	switch strings.ToLower(codec.MimeType) {
	case "video/vp8":
		if len(data) < 10 {
			return nil
		}
		keyframe := (data[0]&0x1 == 0)
		if !keyframe {
			return nil
		}
		raw := uint32(data[6]) | uint32(data[7])<<8 |
			uint32(data[8])<<16 | uint32(data[9])<<24
		width := raw & 0x3FFF
		height := (raw >> 16) & 0x3FFF
		return t.conn.initWriter(width, height)
	}
	return nil
}

// called locked
func (conn *diskConn) initWriter(width, height uint32) error {
	if conn.file != nil && width == conn.width && height == conn.height {
		return nil
	}
	var entries []webm.TrackEntry
	for i, t := range conn.tracks {
		var entry webm.TrackEntry
		codec := t.remote.Codec()
		switch strings.ToLower(codec.MimeType) {
		case "audio/opus":
			entry = webm.TrackEntry{
				Name:        "Audio",
				TrackNumber: uint64(i + 1),
				CodecID:     "A_OPUS",
				TrackType:   2,
				Audio: &webm.Audio{
					SamplingFrequency: float64(codec.ClockRate),
					Channels:          uint64(codec.Channels),
				},
			}
		case "video/vp8":
			entry = webm.TrackEntry{
				Name:        "Video",
				TrackNumber: uint64(i + 1),
				CodecID:     "V_VP8",
				TrackType:   1,
				Video: &webm.Video{
					PixelWidth:  uint64(width),
					PixelHeight: uint64(height),
				},
			}
		default:
			return errors.New("unknown track type")
		}
		entries = append(entries, entry)
	}

	err := conn.reopen()
	if err != nil {
		return err
	}

	writers, err := webm.NewSimpleBlockWriter(conn.file, entries)
	if err != nil {
		conn.file.Close()
		conn.file = nil
		return err
	}

	if len(writers) != len(conn.tracks) {
		conn.file.Close()
		conn.file = nil
		return errors.New("unexpected number of writers")
	}

	conn.width = width
	conn.height = height

	for i, t := range conn.tracks {
		t.writer = writers[i]
	}
	return nil
}

func (down *diskConn) GetMaxBitrate(now uint64) uint64 {
	return ^uint64(0)
}

func (t *diskTrack) Accumulate(bytes uint32) {
	return
}
