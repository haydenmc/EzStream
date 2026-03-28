package main

import (
	"bytes"
	"encoding/binary"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	av1frame "github.com/pion/rtp/codecs/av1/frame"
	"github.com/pion/rtp/codecs/av1/obu"
)

const thumbnailInterval = 10 * time.Second

var h264StartCode = []byte{0, 0, 0, 1}

// thumbnailExtractor processes an RTP video stream and periodically generates
// JPEG thumbnails by running FFmpeg on extracted keyframes. Not safe for
// concurrent use.
type thumbnailExtractor struct {
	mimeType string
	lastGen  time.Time

	// H264 state
	h264SPS []byte // most recently seen SPS NAL unit
	h264PPS []byte // most recently seen PPS NAL unit
	h264FU  []byte // FU-A reassembly buffer

	// AV1 state
	av1Asm      av1frame.AV1 // OBU element reassembler (handles Z/Y fragmentation)
	av1Buf      bytes.Buffer
	av1InSeq    bool   // true while accumulating a coded video sequence
	av1HasSH    bool   // current accumulation contains a Sequence Header OBU
	av1HasFrame bool   // current accumulation contains frame data
	av1SH       []byte // cached Sequence Header OBU to prepend when SH is absent
}

func newThumbnailExtractor(mimeType string) *thumbnailExtractor {
	return &thumbnailExtractor{mimeType: strings.ToLower(mimeType)}
}

// Feed processes one raw RTP packet (header + payload) and returns JPEG bytes
// when a new thumbnail has been generated, or nil otherwise.
func (e *thumbnailExtractor) Feed(rawRTP []byte) []byte {
	if time.Since(e.lastGen) < thumbnailInterval {
		return nil
	}
	var pkt rtp.Packet
	if err := pkt.Unmarshal(rawRTP); err != nil {
		return nil
	}
	switch e.mimeType {
	case "video/h264":
		return e.feedH264(&pkt)
	case "video/av1":
		return e.feedAV1(&pkt)
	}
	return nil
}

// feedH264 depacketizes one H264 RTP packet and generates a thumbnail when a
// complete IDR frame is available.
func (e *thumbnailExtractor) feedH264(pkt *rtp.Packet) []byte {
	payload := pkt.Payload
	if len(payload) == 0 {
		return nil
	}
	naluType := payload[0] & 0x1f
	switch {
	case naluType >= 1 && naluType <= 23:
		// Single NAL unit packet
		return e.processH264NAL(payload)

	case naluType == 24:
		// STAP-A: multiple NAL units bundled in one packet
		off := 1
		for off+2 <= len(payload) {
			size := int(payload[off])<<8 | int(payload[off+1])
			off += 2
			if off+size > len(payload) {
				break
			}
			if jpeg := e.processH264NAL(payload[off : off+size]); jpeg != nil {
				return jpeg
			}
			off += size
		}

	case naluType == 28:
		// FU-A: NAL unit fragmented across multiple packets
		if len(payload) < 2 {
			return nil
		}
		fuHeader := payload[1]
		if fuHeader&0x80 != 0 { // start bit
			// Reconstruct the NAL header from the forbidden_zero_bit+NRI of the
			// FU indicator and the nal_unit_type from the FU header.
			e.h264FU = []byte{(payload[0] & 0xe0) | (fuHeader & 0x1f)}
		}
		if len(e.h264FU) == 0 {
			return nil
		}
		e.h264FU = append(e.h264FU, payload[2:]...)
		if fuHeader&0x40 != 0 { // end bit
			nal := e.h264FU
			e.h264FU = nil
			return e.processH264NAL(nal)
		}
	}
	return nil
}

// processH264NAL inspects a single NAL unit. It caches SPS/PPS and returns
// JPEG bytes when an IDR frame arrives with both SPS and PPS available.
func (e *thumbnailExtractor) processH264NAL(nal []byte) []byte {
	if len(nal) == 0 {
		return nil
	}
	switch nal[0] & 0x1f {
	case 7: // SPS
		e.h264SPS = append([]byte(nil), nal...)
	case 8: // PPS
		e.h264PPS = append([]byte(nil), nal...)
	case 5: // IDR (keyframe)
		if len(e.h264SPS) == 0 || len(e.h264PPS) == 0 {
			return nil
		}
		var frame bytes.Buffer
		frame.Write(h264StartCode)
		frame.Write(e.h264SPS)
		frame.Write(h264StartCode)
		frame.Write(e.h264PPS)
		frame.Write(h264StartCode)
		frame.Write(nal)
		return e.runFFmpeg("h264", frame.Bytes())
	}
	return nil
}

// feedAV1 depacketizes one AV1 RTP packet and generates a thumbnail once a
// complete keyframe is available.
//
// Uses AV1Packet + frame.AV1 for element-level reassembly (handles Z/Y
// fragmentation). Each assembled OBU is sanitized before buffering:
// obu_reserved_1bit is cleared (OBS sets it to 1 which libdav1d rejects) and
// obu_has_size_field is set to 1 for valid low-overhead bitstream output.
//
// The Sequence Header is cached so it can be prepended to keyframes that OBS
// sends without one (PLI-triggered keyframes sometimes omit the SH).
func (e *thumbnailExtractor) feedAV1(pkt *rtp.Packet) []byte {
	var ap codecs.AV1Packet //nolint:staticcheck
	if _, err := ap.Unmarshal(pkt.Payload); err != nil {
		return nil
	}
	if ap.N {
		// N=1 marks the start of a new coded video sequence.
		e.av1Asm = av1frame.AV1{}
		e.av1Buf.Reset()
		e.av1InSeq = true
		e.av1HasSH = false
		e.av1HasFrame = false
	}
	if !e.av1InSeq {
		return nil
	}
	assembled, err := e.av1Asm.ReadFrames(&ap) //nolint:staticcheck
	if err != nil {
		return nil
	}
	for _, raw := range assembled {
		e.bufferAV1OBU(raw)
	}
	// The RTP marker bit is set on the last packet of each temporal unit.
	// Only emit once we have actual frame data.
	if pkt.Marker && e.av1HasFrame {
		data := make([]byte, e.av1Buf.Len())
		copy(data, e.av1Buf.Bytes())
		e.av1Buf.Reset()
		e.av1InSeq = false

		slog.Info("Thumbnail AV1: emitting to FFmpeg",
			"hasSH", e.av1HasSH, "cachedSH", len(e.av1SH), "dataLen", len(data))

		if !e.av1HasSH {
			if len(e.av1SH) == 0 {
				return nil
			}
			var buf bytes.Buffer
			buf.Write(e.av1SH)
			buf.Write(data)
			data = buf.Bytes()
		}
		return e.runFFmpeg("av1", data)
	}
	return nil
}

// bufferAV1OBU sanitizes one raw OBU element (as returned by frame.AV1.ReadFrames)
// and appends it to av1Buf. Ensures obu_has_size_field=1 and clears the reserved
// bit that OBS's AV1 encoder sets to 1 (violating spec; libdav1d hard-fails on it).
func (e *thumbnailExtractor) bufferAV1OBU(raw []byte) {
	if len(raw) == 0 {
		return
	}
	hdr, err := obu.ParseOBUHeader(raw)
	if err != nil {
		return
	}
	if hdr.Type == obu.OBUTemporalDelimiter || hdr.Type == obu.OBUTileList {
		return
	}

	// Extract the payload, handling both sized and unsized OBU headers.
	var payload []byte
	if hdr.HasSizeField {
		size, n, err := obu.ReadLeb128(raw[hdr.Size():])
		if err != nil {
			return
		}
		end := hdr.Size() + int(n) + int(size)
		if end > len(raw) {
			return
		}
		payload = raw[hdr.Size()+int(n) : end]
	} else {
		payload = raw[hdr.Size():]
	}

	// Rebuild the header: set size field, clear reserved bit.
	hdr.HasSizeField = true
	hdr.Reserved1Bit = false
	headerBytes := hdr.Marshal()
	sizeBytes := obu.WriteToLeb128(uint(len(payload)))

	switch hdr.Type {
	case obu.OBUSequenceHeader:
		e.av1HasSH = true
		sh := make([]byte, 0, len(headerBytes)+len(sizeBytes)+len(payload))
		sh = append(sh, headerBytes...)
		sh = append(sh, sizeBytes...)
		sh = append(sh, payload...)
		e.av1SH = sh
	case obu.OBUFrame, obu.OBUFrameHeader, obu.OBUTileGroup:
		e.av1HasFrame = true
	}

	e.av1Buf.Write(headerBytes)
	e.av1Buf.Write(sizeBytes)
	e.av1Buf.Write(payload)
}

// buildIVF wraps raw AV1 OBU data (one temporal unit) in an IVF container.
// FFmpeg's ivf demuxer is reliable with AV1 whereas the raw av1 demuxer can
// fail to locate the Sequence Header when the bitstream has no Annex B framing.
func buildIVF(data []byte) []byte {
	buf := make([]byte, 32+12+len(data))
	// File header (32 bytes)
	copy(buf[0:], "DKIF")
	binary.LittleEndian.PutUint16(buf[4:], 0)       // version
	binary.LittleEndian.PutUint16(buf[6:], 32)      // header length
	copy(buf[8:], "AV01")                            // codec FourCC
	binary.LittleEndian.PutUint16(buf[12:], 0)      // width (FFmpeg reads from SH)
	binary.LittleEndian.PutUint16(buf[14:], 0)      // height
	binary.LittleEndian.PutUint32(buf[16:], 1)      // timebase numerator
	binary.LittleEndian.PutUint32(buf[20:], 1)      // timebase denominator
	binary.LittleEndian.PutUint32(buf[24:], 1)      // total frames
	binary.LittleEndian.PutUint32(buf[28:], 0)      // unused
	// Frame header (12 bytes)
	binary.LittleEndian.PutUint32(buf[32:], uint32(len(data)))
	binary.LittleEndian.PutUint64(buf[36:], 0) // pts
	// Frame data
	copy(buf[44:], data)
	return buf
}

func (e *thumbnailExtractor) runFFmpeg(codec string, data []byte) []byte {
	// AV1: wrap OBU data in an IVF container so FFmpeg's ivf demuxer is used
	// instead of the raw av1 demuxer (which expects Annex B framing). IVF is
	// fully sequential so piped input works without seeking.
	inputFormat := codec
	if codec == "av1" {
		inputFormat = "ivf"
		data = buildIVF(data)
	}

	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", inputFormat,
		"-i", "pipe:0",
		"-frames:v", "1",
		"-f", "image2",
		"-vcodec", "mjpeg",
		"-q:v", "5",
		"pipe:1",
	)
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		slog.Error("Thumbnail: FFmpeg failed", "codec", codec, "error", err,
			"ffmpeg_stderr", stderr.String())
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	slog.Info("Thumbnail: Generated", "codec", codec, "bytes", len(out))
	e.lastGen = time.Now()
	return out
}
