// Opus packet framing (RFC 6716 §3) and M1's audio Marker (internal/media/fake/marker.go), for the test tone's
// packets (docs/m2/04-windows-engine.md §10.3): under IM_TEST_HOOKS with IM_ENC_FAKE, the Opus stage re-frames each
// tone packet as a code 3 packet whose padding is the 18-byte Marker, exactly as fake.Source's Opus packets are
// framed, so that M1's fake.ParseAudioMarker and sfutest.Viewer read native packets as they read the fake's.
// Decoders ignore padding, so the audio is unchanged.
#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <span>

namespace im::audio {

// ---------------------------------------------------------------------------------------------------- the Marker

// The size of an encoded Marker: magic "ISHN" (4), rid (1, 0 for audio), flags (1), frame index (uint32) and capture
// ns (uint64), big endian (M1's fake.MarkerSize).
inline constexpr std::size_t kMarkerSize = 18;

// What a fake frame or audio packet says about itself (M1's fake.Marker). The test tone's packets carry rid 0,
// Frame = the packet's index from 0, CaptureNS = capture_ts_ns - epoch_ns and Beep on the first packet whose capture
// time is at or after each beep start (epoch_ns + k * 1 s).
struct Marker {
  char rid = 0; // the layer's RID byte; 0 for audio
  bool keyframe = false;
  bool flash = false;
  bool beep = false;
  uint32_t frame = 0;
  int64_t capture_ns = 0;

  bool operator==(const Marker &) const = default;
};

// Writes m's 18 bytes.
void EncodeMarker(const Marker &m, std::span<uint8_t, kMarkerSize> out) noexcept;
// Reads a Marker from at least 18 bytes; false without the magic.
bool DecodeMarker(std::span<const uint8_t> b, Marker *m) noexcept;

// ------------------------------------------------------------------------------------------------- Opus framing

inline constexpr std::size_t kMaxOpusFrameBytes = 1275; // RFC 6716 R2
inline constexpr std::size_t kMaxOpusFrames = 48;       // 120 ms of 2.5 ms frames (R5)

// One Opus packet split into its frames (RFC 6716 §3). The frames view the packet.
struct OpusFrames {
  uint8_t toc = 0;
  uint32_t count = 0; // frames in use
  std::array<std::span<const uint8_t>, kMaxOpusFrames> frames{};
  uint32_t padding = 0; // bytes of padding (code 3)
};

// Splits pkt into its frames and checks the framing requirements R1-R7 of RFC 6716 §3.4, as M1's parseOpus does.
// False for a malformed packet.
bool ParseOpusPacket(std::span<const uint8_t> pkt, OpusFrames *out) noexcept;

// The size of pkt's frames re-framed as a code 3 packet with pad_bytes of padding, or 0 for a malformed pkt.
std::size_t PaddedSize(std::span<const uint8_t> pkt, std::size_t pad_bytes) noexcept;

// Writes pkt's frames as a code 3 packet (RFC 6716 §3.2.5) whose padding is pad: CBR when the frames are the same
// size, else VBR; any padding pkt had is dropped. Byte for byte what M1's appendOpusPadded writes. Returns the bytes
// written, or 0 when pkt is malformed or out is smaller than PaddedSize(pkt, pad.size()).
std::size_t WritePadded(std::span<const uint8_t> pkt, std::span<const uint8_t> pad, std::span<uint8_t> out) noexcept;

// WritePadded with m's 18 bytes as the padding: the test tone's framing.
std::size_t WriteWithMarker(std::span<const uint8_t> pkt, const Marker &m, std::span<uint8_t> out) noexcept;

// The Marker at the start of a code 3 packet's padding, as M1's fake.ParseAudioMarker reads it. False for any other
// packet, including valid Opus without the Marker.
bool ParseAudioMarker(std::span<const uint8_t> pkt, Marker *m) noexcept;

} // namespace im::audio
