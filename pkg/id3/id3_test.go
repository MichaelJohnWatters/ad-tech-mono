package id3

import (
	"bytes"
	"testing"
)

func desynchsafe(b []byte) int {
	return int(b[0])<<21 | int(b[1])<<14 | int(b[2])<<7 | int(b[3])
}

// TestSynchsafeBoundaries checks the 28-bit range guard: the max value encodes,
// and anything larger (or negative) panics rather than silently truncating the
// size field into a corrupt tag.
func TestSynchsafeBoundaries(t *testing.T) {
	// Max 28-bit value round-trips.
	if got := desynchsafe(synchsafe(0x0FFFFFFF)); got != 0x0FFFFFFF {
		t.Errorf("synchsafe(0x0FFFFFFF) round-trip = %d, want %d", got, 0x0FFFFFFF)
	}
	for _, n := range []int{0x10000000, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("synchsafe(%d) did not panic on out-of-range size", n)
				}
			}()
			synchsafe(n)
		}()
	}
}

func TestEncodeQuartileTag(t *testing.T) {
	tag := QuartileTag("firstQuartile")

	// Header: "ID3", v2.4.0, flags 0, synchsafe size = len(body).
	if string(tag[0:3]) != "ID3" {
		t.Fatalf("bad magic: %q", tag[0:3])
	}
	if tag[3] != 0x04 || tag[4] != 0x00 {
		t.Errorf("version = %d.%d, want 4.0", tag[3], tag[4])
	}
	size := desynchsafe(tag[6:10])
	if size != len(tag)-10 {
		t.Errorf("tag size = %d, want %d", size, len(tag)-10)
	}
	// Synchsafe bytes must all have the high bit clear.
	for _, sb := range tag[6:10] {
		if sb&0x80 != 0 {
			t.Errorf("size byte %#x not synchsafe", sb)
		}
	}

	// Frame: TXXX, synchsafe frame size, UTF-8, description\0value.
	fr := tag[10:]
	if string(fr[0:4]) != "TXXX" {
		t.Fatalf("frame id = %q, want TXXX", fr[0:4])
	}
	fsize := desynchsafe(fr[4:8])
	data := fr[10 : 10+fsize]
	if data[0] != 0x03 {
		t.Errorf("text encoding = %d, want 3 (UTF-8)", data[0])
	}
	nul := bytes.IndexByte(data[1:], 0x00)
	if nul < 0 {
		t.Fatal("no description terminator")
	}
	desc := string(data[1 : 1+nul])
	val := string(data[1+nul+1:])
	if desc != Scheme {
		t.Errorf("description = %q, want %q", desc, Scheme)
	}
	if val != "firstQuartile" {
		t.Errorf("value = %q, want firstQuartile", val)
	}
}

func TestPRIV(t *testing.T) {
	tag := Encode(PRIV("adtech", []byte{1, 2, 3}))
	fr := tag[10:]
	if string(fr[0:4]) != "PRIV" {
		t.Fatalf("frame id = %q, want PRIV", fr[0:4])
	}
	fsize := desynchsafe(fr[4:8])
	data := fr[10 : 10+fsize]
	nul := bytes.IndexByte(data, 0x00)
	if string(data[:nul]) != "adtech" || !bytes.Equal(data[nul+1:], []byte{1, 2, 3}) {
		t.Errorf("PRIV owner/data wrong: %v", data)
	}
}

func TestEncodeMultiFrame(t *testing.T) {
	tag := Encode(TXXX("a", "1"), TXXX("b", "22"))
	if desynchsafe(tag[6:10]) != len(tag)-10 {
		t.Error("multi-frame tag size mismatch")
	}
}
