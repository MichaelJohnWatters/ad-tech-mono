// Package id3 is a minimal ID3v2.4 tag encoder for in-band HLS/CMAF timed
// metadata. Players (hls.js FRAG_PARSING_METADATA, native CTV) read ID3 frames
// embedded in the media segments and fire beacons at playback time — the
// in-band alternative to the manifest-level DASH EventStream / HLS DATERANGE.
//
// This package is the tested encoder core. Embedding a tag into the actual
// segment bytes at a PTS offset (a TS metadata PES on its own PID, or a CMAF
// emsg/metadata track) is a separate muxing step that consumes Encode()'s output
// — see docs/SSAI_CONDITIONING.md (R5 / in-band ID3).
package id3

import "fmt"

// Frame is one ID3v2 frame: a 4-char ID and its payload bytes.
type Frame struct {
	ID   string
	Data []byte
}

// TXXX builds a user-defined text frame (description + value), UTF-8 encoded.
// SSAI uses it to carry the quartile event name keyed by a scheme description.
func TXXX(description, value string) Frame {
	var b []byte
	b = append(b, 0x03) // text encoding: UTF-8
	b = append(b, []byte(description)...)
	b = append(b, 0x00) // description terminator
	b = append(b, []byte(value)...)
	return Frame{ID: "TXXX", Data: b}
}

// PRIV builds a private frame: an owner identifier + arbitrary private data.
func PRIV(owner string, data []byte) Frame {
	var b []byte
	b = append(b, []byte(owner)...)
	b = append(b, 0x00) // owner terminator
	b = append(b, data...)
	return Frame{ID: "PRIV", Data: b}
}

// Encode serialises frames into a complete ID3v2.4 tag (header + frames). Frame
// and tag sizes use the synchsafe integer encoding the ID3 spec requires.
func Encode(frames ...Frame) []byte {
	var body []byte
	for _, f := range frames {
		id := (f.ID + "    ")[:4]
		body = append(body, []byte(id)...)
		body = append(body, synchsafe(len(f.Data))...)
		body = append(body, 0x00, 0x00) // frame flags
		body = append(body, f.Data...)
	}
	tag := []byte{'I', 'D', '3', 0x04, 0x00, 0x00} // "ID3" v2.4.0, no flags
	tag = append(tag, synchsafe(len(body))...)
	return append(tag, body...)
}

// QuartileTag is the SSAI convenience: an ID3 tag carrying a single VAST
// quartile event under the SSAI scheme, ready to embed at that quartile's PTS.
func QuartileTag(event string) []byte {
	return Encode(TXXX(Scheme, event))
}

// Scheme is the TXXX description SSAI quartile events are keyed by (matches the
// DASH EventStream schemeIdUri).
const Scheme = "urn:adtech:ssai:quartile"

// synchsafe encodes n as a 4-byte synchsafe integer (7 bits per byte, high bit
// clear) — the format ID3 uses so tag data can't be mistaken for an MPEG sync.
// A 4-byte synchsafe int holds 28 bits, so n must be in [0, 0x0FFFFFFF]; a larger
// value would silently drop its high bits and write a corrupt (too-small) size,
// so we panic with a clear message instead. Not reachable for SSAI quartile tags
// (payloads are a few bytes), but this is a public encoder.
func synchsafe(n int) []byte {
	if n < 0 || n > 0x0FFFFFFF {
		panic(fmt.Sprintf("id3: size %d out of range for a 28-bit synchsafe integer [0, %d]", n, 0x0FFFFFFF))
	}
	return []byte{
		byte((n >> 21) & 0x7f),
		byte((n >> 14) & 0x7f),
		byte((n >> 7) & 0x7f),
		byte(n & 0x7f),
	}
}
