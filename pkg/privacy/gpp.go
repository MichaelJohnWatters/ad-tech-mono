package privacy

import (
	"encoding/base64"
	"sort"
	"strconv"
	"strings"
)

// GPP (IAB Global Privacy Platform) minimal decode.
//
// A GPP string is `header~section0~section1~…`. The header lists which sections
// are present (also mirrored in the OpenRTB regs.ext.gpp_sid field), and each
// section is a base64url-encoded bitfield with its own schema. We decode only
// what gates personalisation: the sale / sharing / targeted-advertising opt-out
// bits of the **US National** section (GPP section id 7), whose core-segment
// layout is fixed and stable.
//
// Scope note: other US state sections (8–12) have similar but not identical
// layouts; they're recognised but not yet decoded (treated as "no signal").
// Adding GPP support is a strict privacy improvement over ignoring GPP
// entirely, and a GPP-derived opt-out only downgrades a bid to contextual
// (never a hard no-bid), so an unsupported/undecoded section is safe. The bit
// offsets below follow the IAB "GPP US National" technical spec and should be
// cross-checked against official test vectors before being relied on for
// strict (reject) enforcement.
const gppSectionUSNational = 7

// GPPOptOut reports whether a supported GPP section signals that the user has
// opted out of sale, sharing, or targeted advertising. gppSID is the OpenRTB
// regs.ext.gpp_sid list. Unparseable input or unsupported sections yield false
// — like usPrivacyOptOut, we never fabricate an opt-out from a bad value.
func GPPOptOut(gpp, gppSID string) bool {
	if gpp == "" {
		return false
	}
	parts := strings.Split(gpp, "~")
	if len(parts) < 2 {
		return false // header only, no sections
	}
	sections := parts[1:]

	// Sections in the GPP string are ordered by ascending section id, matching
	// the sorted gpp_sid list — zip them so we know each section's id.
	ids := parseSectionIDs(gppSID)
	for i, sec := range sections {
		if i >= len(ids) {
			break
		}
		if ids[i] == gppSectionUSNational && usNationalOptOut(sec) {
			return true
		}
	}
	return false
}

// parseSectionIDs parses regs.ext.gpp_sid (e.g. "7,8" or "7 8") into a sorted
// ascending slice of section ids, matching the section order in the GPP string.
func parseSectionIDs(gppSID string) []int {
	fields := strings.FieldsFunc(gppSID, func(r rune) bool {
		return r == ',' || r == ' '
	})
	ids := make([]int, 0, len(fields))
	for _, f := range fields {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			ids = append(ids, n)
		}
	}
	sort.Ints(ids)
	return ids
}

// usNationalOptOut decodes the US National core segment and reports whether the
// SaleOptOut, SharingOptOut, or TargetedAdvertisingOptOut field is set to
// "opted out" (value 1; 0 = N/A, 2 = did not opt out).
//
// Core-segment layout (each field's width in bits):
//
//	Version                              6
//	SharingNotice                        2
//	SaleOptOutNotice                     2
//	SharingOptOutNotice                  2
//	TargetedAdvertisingOptOutNotice      2
//	SensitiveDataProcessingOptOutNotice  2
//	SensitiveDataLimitUseNotice          2
//	SaleOptOut                           2   ← bit offset 18
//	SharingOptOut                        2   ← bit offset 20
//	TargetedAdvertisingOptOut            2   ← bit offset 22
func usNationalOptOut(section string) bool {
	// A section may carry sub-segments joined by ".", but the core segment is
	// always first.
	core := section
	if i := strings.IndexByte(section, '.'); i >= 0 {
		core = section[:i]
	}
	data, err := base64.RawURLEncoding.DecodeString(core)
	if err != nil || len(data)*8 < 24 {
		return false
	}
	br := bitReader{data: data}
	br.skip(6)  // Version
	br.skip(12) // six 2-bit notice fields
	sale := br.read(2)
	sharing := br.read(2)
	targeted := br.read(2)
	const optedOut = 1
	return sale == optedOut || sharing == optedOut || targeted == optedOut
}

// bitReader reads big-endian bit fields from a byte slice, MSB first — the
// encoding IAB uses for GPP / TCF sections.
type bitReader struct {
	data []byte
	pos  int // current bit position
}

func (b *bitReader) skip(n int) { b.pos += n }

func (b *bitReader) read(n int) int {
	v := 0
	for i := 0; i < n; i++ {
		byteIdx := b.pos / 8
		bit := 0
		if byteIdx < len(b.data) {
			bitIdx := 7 - (b.pos % 8)
			bit = int((b.data[byteIdx] >> bitIdx) & 1)
		}
		v = (v << 1) | bit
		b.pos++
	}
	return v
}
