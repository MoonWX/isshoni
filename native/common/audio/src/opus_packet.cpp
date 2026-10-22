// Opus framing and M1's audio Marker (opus_packet.h). The parser and the code 3 writer follow M1's Go code
// (internal/media/fake/opus.go: parseOpus, appendOpusPadded) so that both write the same bytes for the same frames.
#include "im/audio/opus_packet.h"

#include <cstring>

namespace im::audio {
namespace {

constexpr uint8_t kMagic[4] = {'I', 'S', 'H', 'N'};
constexpr uint8_t kFlagKeyframe = 1, kFlagFlash = 2, kFlagBeep = 4;
constexpr uint32_t kMaxPacketSamples = 120 * 48; // 120 ms at 48 kHz (R5)

// The duration of one frame of a TOC byte's configuration at 48 kHz (RFC 6716 §3.1).
uint32_t FrameSamples(uint8_t toc) noexcept {
  const uint32_t config = toc >> 3;
  if (config < 12) { // SILK: 10, 20, 40, 60 ms
    constexpr uint32_t kSilk[4] = {480, 960, 1920, 2880};
    return kSilk[config & 3];
  }
  if (config < 16) { // Hybrid: 10, 20 ms
    return (config & 1) != 0 ? 960 : 480;
  }
  constexpr uint32_t kCelt[4] = {120, 240, 480, 960}; // CELT: 2.5, 5, 10, 20 ms
  return kCelt[config & 3];
}

// Reads a frame length (RFC 6716 §3.2.1): one byte below 252, else two bytes.
bool ReadFrameLength(std::span<const uint8_t> b, std::size_t *n, std::size_t *used) noexcept {
  if (b.empty()) {
    return false;
  }
  if (b[0] < 252) {
    *n = b[0];
    *used = 1;
    return true;
  }
  if (b.size() < 2) {
    return false;
  }
  *n = 4 * std::size_t{b[1]} + b[0];
  *used = 2;
  return true;
}

std::size_t FrameLengthBytes(std::size_t n) noexcept { return n < 252 ? 1 : 2; }

// The bytes of a padding length (RFC 6716 §3.2.5): each 255 stands for 254 bytes and another length byte follows.
std::size_t PadLengthBytes(std::size_t pad) noexcept {
  std::size_t bytes = 1;
  for (; pad >= 255; pad -= 254) {
    bytes++;
  }
  return bytes;
}

// Whether a code 3 packet of these frames is VBR (the frames differ in size).
bool IsVbr(const OpusFrames &f) noexcept {
  for (uint32_t i = 1; i < f.count; i++) {
    if (f.frames[i].size() != f.frames[0].size()) {
      return true;
    }
  }
  return false;
}

} // namespace

void EncodeMarker(const Marker &m, std::span<uint8_t, kMarkerSize> out) noexcept {
  std::memcpy(out.data(), kMagic, sizeof kMagic);
  out[4] = static_cast<uint8_t>(m.rid);
  uint8_t flags = 0;
  if (m.keyframe) {
    flags |= kFlagKeyframe;
  }
  if (m.flash) {
    flags |= kFlagFlash;
  }
  if (m.beep) {
    flags |= kFlagBeep;
  }
  out[5] = flags;
  for (int i = 0; i < 4; i++) {
    out[6 + i] = static_cast<uint8_t>(m.frame >> (24 - 8 * i));
  }
  const auto ns = static_cast<uint64_t>(m.capture_ns);
  for (int i = 0; i < 8; i++) {
    out[10 + i] = static_cast<uint8_t>(ns >> (56 - 8 * i));
  }
}

bool DecodeMarker(std::span<const uint8_t> b, Marker *m) noexcept {
  if (m == nullptr || b.size() < kMarkerSize || std::memcmp(b.data(), kMagic, sizeof kMagic) != 0) {
    return false;
  }
  Marker out;
  out.rid = static_cast<char>(b[4]);
  out.keyframe = (b[5] & kFlagKeyframe) != 0;
  out.flash = (b[5] & kFlagFlash) != 0;
  out.beep = (b[5] & kFlagBeep) != 0;
  for (int i = 0; i < 4; i++) {
    out.frame = (out.frame << 8) | b[6 + i];
  }
  uint64_t ns = 0;
  for (int i = 0; i < 8; i++) {
    ns = (ns << 8) | b[10 + i];
  }
  out.capture_ns = static_cast<int64_t>(ns);
  *m = out;
  return true;
}

bool ParseOpusPacket(std::span<const uint8_t> pkt, OpusFrames *out) noexcept {
  if (out == nullptr || pkt.empty()) { // R1
    return false;
  }
  OpusFrames p;
  p.toc = pkt[0];
  auto rest = pkt.subspan(1);
  switch (pkt[0] & 3) {
  case 0:
    p.count = 1;
    p.frames[0] = rest;
    break;
  case 1:
    if (rest.size() % 2 != 0) { // R3
      return false;
    }
    p.count = 2;
    p.frames[0] = rest.first(rest.size() / 2);
    p.frames[1] = rest.subspan(rest.size() / 2);
    break;
  case 2: {
    std::size_t n = 0, used = 0;
    if (!ReadFrameLength(rest, &n, &used) || n > rest.size() - used) { // R4
      return false;
    }
    p.count = 2;
    p.frames[0] = rest.subspan(used, n);
    p.frames[1] = rest.subspan(used + n);
    break;
  }
  default: {
    if (rest.empty()) { // R6
      return false;
    }
    const bool vbr = (rest[0] & 0x80) != 0, padded = (rest[0] & 0x40) != 0;
    const uint32_t count = rest[0] & 0x3fu;
    rest = rest.subspan(1);
    if (count == 0 || count * FrameSamples(p.toc) > kMaxPacketSamples) { // R5
      return false;
    }
    if (padded) {
      std::size_t pad = 0;
      for (;;) {
        if (rest.empty()) { // R6, R7
          return false;
        }
        const std::size_t c = rest[0];
        rest = rest.subspan(1);
        if (c < 255) {
          pad += c;
          break;
        }
        pad += 254;
      }
      if (pad > rest.size()) { // R6, R7
        return false;
      }
      rest = rest.first(rest.size() - pad);
      p.padding = static_cast<uint32_t>(pad);
    }
    p.count = count;
    if (!vbr) {
      if (rest.size() % count != 0) { // R6
        return false;
      }
      const std::size_t n = rest.size() / count;
      for (uint32_t i = 0; i < count; i++) {
        p.frames[i] = rest.subspan(i * n, n);
      }
      break;
    }
    std::size_t lengths[kMaxOpusFrames] = {};
    for (uint32_t i = 0; i + 1 < count; i++) {
      std::size_t used = 0;
      if (!ReadFrameLength(rest, &lengths[i], &used)) { // R7
        return false;
      }
      rest = rest.subspan(used);
    }
    for (uint32_t i = 0; i + 1 < count; i++) {
      if (lengths[i] > rest.size()) { // R7
        return false;
      }
      p.frames[i] = rest.first(lengths[i]);
      rest = rest.subspan(lengths[i]);
    }
    p.frames[count - 1] = rest;
    break;
  }
  }
  for (uint32_t i = 0; i < p.count; i++) {
    if (p.frames[i].size() > kMaxOpusFrameBytes) { // R2
      return false;
    }
  }
  *out = p;
  return true;
}

std::size_t PaddedSize(std::span<const uint8_t> pkt, std::size_t pad_bytes) noexcept {
  OpusFrames f;
  if (!ParseOpusPacket(pkt, &f)) {
    return 0;
  }
  std::size_t n = 2 + PadLengthBytes(pad_bytes) + pad_bytes; // TOC, frame count, padding length, padding
  const bool vbr = IsVbr(f);
  for (uint32_t i = 0; i < f.count; i++) {
    n += f.frames[i].size();
    if (vbr && i + 1 < f.count) {
      n += FrameLengthBytes(f.frames[i].size());
    }
  }
  return n;
}

std::size_t WritePadded(std::span<const uint8_t> pkt, std::span<const uint8_t> pad, std::span<uint8_t> out) noexcept {
  OpusFrames f;
  if (!ParseOpusPacket(pkt, &f)) {
    return 0;
  }
  const std::size_t size = PaddedSize(pkt, pad.size());
  if (size == 0 || out.size() < size) {
    return 0;
  }
  const bool vbr = IsVbr(f);
  std::size_t at = 0;
  out[at++] = static_cast<uint8_t>(f.toc | 3);
  out[at++] = static_cast<uint8_t>(0x40u | f.count | (vbr ? 0x80u : 0u)); // p = 1
  std::size_t n = pad.size();
  for (; n >= 255; n -= 254) {
    out[at++] = 255;
  }
  out[at++] = static_cast<uint8_t>(n);
  if (vbr) {
    for (uint32_t i = 0; i + 1 < f.count; i++) {
      const std::size_t len = f.frames[i].size();
      if (len < 252) {
        out[at++] = static_cast<uint8_t>(len);
      } else {
        const std::size_t first = 252 + ((len - 252) & 3);
        out[at++] = static_cast<uint8_t>(first);
        out[at++] = static_cast<uint8_t>((len - first) / 4);
      }
    }
  }
  for (uint32_t i = 0; i < f.count; i++) {
    if (!f.frames[i].empty()) {
      std::memcpy(out.data() + at, f.frames[i].data(), f.frames[i].size());
      at += f.frames[i].size();
    }
  }
  if (!pad.empty()) {
    std::memcpy(out.data() + at, pad.data(), pad.size());
    at += pad.size();
  }
  return at;
}

std::size_t WriteWithMarker(std::span<const uint8_t> pkt, const Marker &m, std::span<uint8_t> out) noexcept {
  uint8_t pad[kMarkerSize];
  EncodeMarker(m, pad);
  return WritePadded(pkt, pad, out);
}

bool ParseAudioMarker(std::span<const uint8_t> pkt, Marker *m) noexcept {
  if (m == nullptr || pkt.size() < 2 || (pkt[0] & 3) != 3 || (pkt[1] & 0x40) == 0 || (pkt[1] & 0x3f) == 0) {
    return false;
  }
  // Padding length (RFC 6716 §3.2.5): each 255 adds 254 bytes and another length byte follows.
  std::size_t pad = 0, i = 2;
  for (;;) {
    if (i >= pkt.size()) {
      return false;
    }
    const std::size_t c = pkt[i++];
    if (c < 255) {
      pad += c;
      break;
    }
    pad += 254;
  }
  if (pad < kMarkerSize || pad > pkt.size() - i) {
    return false;
  }
  return DecodeMarker(pkt.subspan(pkt.size() - pad), m);
}

} // namespace im::audio
