package rtsp

import (
	"encoding/hex"
)

// defaultProfileLevelID is the safe fallback (Constrained Baseline, Level 3.1)
// used when an SPS NAL is unavailable or malformed. Most WebRTC endpoints
// accept it.
const defaultProfileLevelID = "42e01f"

// profileLevelIDFromSPS derives the H.264 RFC 6184 `profile-level-id`
// fmtp value from a raw SPS NAL (no Annex-B start code, no emulation
// prevention bytes are required because we only look at the first three
// bytes after the NAL header).
//
// SPS layout (RBSP):
//
//	byte 0 : forbidden_zero_bit(1) | nal_ref_idc(2) | nal_unit_type(5)  = 0x67 for SPS
//	byte 1 : profile_idc
//	byte 2 : constraint_set flags + reserved
//	byte 3 : level_idc
//
// Some encoders also emit "naked" SPS RBSP without the NAL header byte; we
// accept both shapes by sniffing for nal_unit_type==7 (0x67).
func profileLevelIDFromSPS(sps []byte) string {
	if len(sps) >= 4 && sps[0]&0x1f == 7 {
		return hex.EncodeToString([]byte{sps[1], sps[2], sps[3]})
	}
	if len(sps) >= 3 {
		// Tolerate NAL-header-stripped SPS.
		return hex.EncodeToString(sps[:3])
	}
	return defaultProfileLevelID
}
