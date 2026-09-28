package main

import (
	"bytes"
	"os/exec"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestThumbnailExtractor_H264GeneratesInBackground(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	stream, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=30", "-frames:v", "1",
		"-c:v", "libx264", "-bsf:v", "h264_mp4toannexb", "-f", "h264", "pipe:1").Output()
	if err != nil {
		t.Skipf("could not encode test frame: %v", err)
	}

	thumbnails := make(chan []byte, 1)
	e := newThumbnailExtractor("video/H264", func(jpeg []byte) { thumbnails <- jpeg })

	// Feed each Annex B NAL unit as a single-NAL RTP packet.
	stream = bytes.ReplaceAll(stream, []byte{0, 0, 0, 1}, []byte{0, 0, 1})
	for seq, nal := range bytes.Split(stream, []byte{0, 0, 1}) {
		if len(nal) == 0 {
			continue
		}
		raw, err := (&rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: uint16(seq)},
			Payload: nal,
		}).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		e.Feed(raw)
	}

	select {
	case jpeg := <-thumbnails:
		if !bytes.HasPrefix(jpeg, []byte{0xff, 0xd8}) {
			t.Fatal("thumbnail is not a JPEG")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for thumbnail")
	}
}
