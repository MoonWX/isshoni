// The Opus stage, the PCM tap and the test tone's Marker framing (docs/m2/04-windows-engine.md §10.3, §18.1; 05
// §10.1-§10.4): 05's settings read back, 10 -> 20 ms framing, lookahead-corrected packet times exactly 20 ms apart,
// REINIT and DISCONTINUITY, the start-at-epoch_ns drop, a decoded 1 kHz tone at 128 kbit/s, encoded silence at
// 50 packets/s, the PCM tap, live bitrate changes and M1's audio Marker in golden bytes (testdata/
// tone_marker_golden.txt, which marker_golden_test.go reads with M1's fake.ParseAudioMarker).
#include <gtest/gtest.h>

#include <opus.h>

#include <algorithm>
#include <atomic>
#include <cmath>
#include <cstring>
#include <fstream>
#include <functional>
#include <memory>
#include <numbers>
#include <random>
#include <set>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

#include "im/abi/status.h"
#include "im/audio/opus_decoder.h"
#include "im/audio/opus_packet.h"
#include "im/audio/opus_stage.h"
#include "im/audio/test_tone.h"

namespace {

using im::audio::AudioPacket;
using im::audio::kChunkFrames;
using im::audio::kChunkNs;
using im::audio::kChunkSamples;
using im::audio::kFrameNs;
using im::audio::Marker;
using im::audio::OpusStage;
using im::audio::OpusStageConfig;

constexpr int64_t kSecond = 1'000'000'000;
constexpr int64_t kLookaheadNs = 6'500'000; // 312 samples at 48 kHz

struct Recorded {
  uint32_t kind = 0;
  uint32_t flags = 0;
  int64_t ts = 0;
  uint32_t duration_us = 0;
  std::vector<uint8_t> data;
};

// A ring that keeps every packet; Write calls listed in refuse (1-based) are refused, as by a full ring.
class Recorder final : public im::audio::PacketWriter {
public:
  bool Write(const AudioPacket &p) noexcept override {
    calls++;
    if (refuse.count(calls) != 0) {
      return false;
    }
    packets.push_back({p.kind, p.flags, p.capture_ts_ns, p.duration_us, {p.data, p.data + p.size}});
    return true;
  }
  std::vector<Recorded> packets;
  std::set<int> refuse;
  int calls = 0;
};

std::unique_ptr<OpusStage> MakeStage(const OpusStageConfig &cfg) {
  std::unique_ptr<OpusStage> s;
  const im_status st = OpusStage::Create(cfg, &s);
  EXPECT_EQ(st, IM_OK) << im::abi::LastErrorText();
  return s;
}

// A signal: the value of channel ch at sample n of the stream (n counts from the grid's first chunk).
using Signal = std::function<float(int64_t n, int ch)>;

float Silence(int64_t, int) { return 0.0f; }

Signal Sine(double hz, double amp) {
  return [=](int64_t n, int) {
    return static_cast<float>(amp * std::sin(2 * std::numbers::pi * hz * static_cast<double>(n) / 48000.0));
  };
}

Signal Noise(float amp, uint32_t seed) {
  auto rng = std::make_shared<std::mt19937>(seed);
  return [=](int64_t, int) { return std::uniform_real_distribution<float>(-amp, amp)(*rng); };
}

// Feeds `chunks` chunks of a 10 ms grid starting at t0 (chunk k at t0 + k * 10 ms), samples first_n onwards of sig.
void Feed(OpusStage &s, int64_t t0, int chunks, const Signal &sig, int64_t first_n = 0) {
  float buf[kChunkSamples];
  for (int k = 0; k < chunks; k++) {
    for (uint32_t i = 0; i < kChunkFrames; i++) {
      const int64_t n = first_n + int64_t{k} * kChunkFrames + i;
      buf[2 * i] = sig(n, 0);
      buf[2 * i + 1] = sig(n, 1);
    }
    s.OnChunk(buf, t0 + k * kChunkNs);
  }
}

// Decodes packets in order with one decoder (interleaved stereo).
std::vector<float> DecodeAll(const std::vector<Recorded> &pkts) {
  std::unique_ptr<im::audio::OpusPacketDecoder> dec;
  EXPECT_EQ(im::audio::OpusPacketDecoder::Create(&dec), IM_OK);
  std::vector<float> out;
  std::vector<float> buf(2 * im::audio::kMaxDecodeFrames);
  for (const auto &p : pkts) {
    const int32_t n =
        dec->Decode(p.data.data(), static_cast<int32_t>(p.data.size()), buf.data(), im::audio::kMaxDecodeFrames);
    EXPECT_EQ(n, 960) << "a 20 ms packet";
    if (n > 0) {
      out.insert(out.end(), buf.begin(), buf.begin() + 2 * n);
    }
  }
  return out;
}

// The amplitude of the hz component of x (interleaved stereo, channel 0) over [from, from + len) frames.
double ToneAmplitude(const std::vector<float> &x, std::size_t from, std::size_t len, double hz) {
  double re = 0, im = 0;
  for (std::size_t i = 0; i < len; i++) {
    const double ph = 2 * std::numbers::pi * hz * static_cast<double>(from + i) / 48000.0;
    re += x[2 * (from + i)] * std::cos(ph);
    im += x[2 * (from + i)] * std::sin(ph);
  }
  return 2 * std::sqrt(re * re + im * im) / static_cast<double>(len);
}

std::string Hex(std::span<const uint8_t> b) {
  static constexpr char kDigits[] = "0123456789abcdef";
  std::string s;
  for (const uint8_t c : b) {
    s.push_back(kDigits[c >> 4]);
    s.push_back(kDigits[c & 15]);
  }
  return s;
}

std::vector<uint8_t> Unhex(const std::string &s) {
  std::vector<uint8_t> out;
  auto nibble = [](char c) { return c <= '9' ? c - '0' : c - 'a' + 10; };
  for (std::size_t i = 0; i + 1 < s.size(); i += 2) {
    out.push_back(static_cast<uint8_t>(nibble(s[i]) << 4 | nibble(s[i + 1])));
  }
  return out;
}

// ------------------------------------------------------------------------------------------------- settings

TEST(OpusStage, SettingsReadBack) {
  Recorder audio;
  auto s = MakeStage({.audio = &audio}); // bitrate 0 = 128 000
  ASSERT_TRUE(s);
  im::audio::OpusSettings got;
  ASSERT_EQ(s->ReadSettings(&got), IM_OK);
  EXPECT_EQ(got.application, OPUS_APPLICATION_AUDIO);
  EXPECT_EQ(got.sample_rate, 48000);
  EXPECT_EQ(got.bitrate, 128000);
  EXPECT_EQ(got.vbr, 1);
  EXPECT_EQ(got.vbr_constraint, 1); // constrained VBR
  EXPECT_EQ(got.complexity, 10);
  EXPECT_EQ(got.signal, OPUS_SIGNAL_MUSIC);
  EXPECT_EQ(got.max_bandwidth, OPUS_BANDWIDTH_FULLBAND);
  EXPECT_EQ(got.inband_fec, 0);
  EXPECT_EQ(got.packet_loss_perc, 0);
  EXPECT_EQ(got.dtx, 0);
  EXPECT_EQ(got.lsb_depth, 24);
  EXPECT_EQ(got.lookahead, 312);
  EXPECT_EQ(s->lookahead_samples(), 312);
  EXPECT_EQ(s->lookahead_ns(), kLookaheadNs);
  EXPECT_EQ(s->stats().bitrate_bps, 128000u);
  EXPECT_FALSE(s->tap_pcm());

  auto movie = MakeStage({.bitrate_bps = 256000, .frame_ms = 20, .audio = &audio}); // the Movie preset
  ASSERT_TRUE(movie);
  ASSERT_EQ(movie->ReadSettings(&got), IM_OK);
  EXPECT_EQ(got.bitrate, 256000);
}

TEST(OpusStage, CreateChecksTheConfig) {
  Recorder audio, pcm;
  std::unique_ptr<OpusStage> s;
  auto create = [&](OpusStageConfig cfg) { return OpusStage::Create(cfg, &s); };
  EXPECT_EQ(OpusStage::Create({.audio = &audio}, nullptr), IM_ERR_INVALID_ARG);
  EXPECT_EQ(create({.bitrate_bps = 31999, .audio = &audio}), IM_ERR_INVALID_ARG);
  EXPECT_NE(im::abi::LastErrorText().find("31999"), std::string_view::npos) << im::abi::LastErrorText();
  EXPECT_FALSE(s);
  EXPECT_EQ(create({.bitrate_bps = 510001, .audio = &audio}), IM_ERR_INVALID_ARG);
  EXPECT_EQ(create({.frame_ms = 10, .audio = &audio}), IM_ERR_INVALID_ARG); // 10 ms frames are later (M5)
  EXPECT_EQ(create({}), IM_ERR_INVALID_ARG);                                // no audio ring
  EXPECT_EQ(create({.flags = IM_AUDIOF_TAP_PCM, .audio = &audio}), IM_ERR_INVALID_ARG);
  EXPECT_EQ(create({.bitrate_bps = 32000, .audio = &audio}), IM_OK);
  EXPECT_EQ(create({.bitrate_bps = 510000, .audio = &audio}), IM_OK);
  // Without IM_AUDIOF_TAP_PCM a PCM ring is unused; unknown flag bits are ignored.
  EXPECT_EQ(create({.flags = IM_AUDIOF_TEST_FAIL_PROBE, .audio = &audio, .pcm = &pcm}), IM_OK);
  ASSERT_TRUE(s);
  EXPECT_FALSE(s->tap_pcm());
  Feed(*s, kLookaheadNs, 10, Silence);
  EXPECT_EQ(pcm.calls, 0);
}

// --------------------------------------------------------------------------------------- framing and times

TEST(OpusStage, TwoChunksMakeOne20msPacket) {
  Recorder audio;
  auto s = MakeStage({.audio = &audio});
  ASSERT_TRUE(s);
  Feed(*s, kLookaheadNs, 99, Sine(440, 0.3)); // 49 frames and a half
  ASSERT_EQ(audio.packets.size(), 49u);
  for (const auto &p : audio.packets) {
    EXPECT_EQ(p.kind, uint32_t{IM_PACKET_AUDIO});
    EXPECT_EQ(p.duration_us, 20000u);
    EXPECT_GT(p.data.size(), 1u);
    EXPECT_LE(p.data.size(), 1275u);
    EXPECT_EQ(opus_packet_get_nb_samples(p.data.data(), static_cast<opus_int32>(p.data.size()), 48000), 960);
    EXPECT_EQ(opus_packet_get_nb_channels(p.data.data()), 2);
  }
  Feed(*s, kLookaheadNs + 99 * kChunkNs, 1, Sine(440, 0.3), 99 * 480); // the second half
  EXPECT_EQ(audio.packets.size(), 50u);
  EXPECT_EQ(s->stats().packets, 50u);
  EXPECT_EQ(DecodeAll(audio.packets).size(), 50u * 960 * 2);
}

TEST(OpusStage, PacketTimesAreFirstChunkMinusLookahead) {
  Recorder audio;
  constexpr int64_t kEpoch = 7 * kSecond + 3;
  auto s = MakeStage({.epoch_ns = kEpoch, .audio = &audio});
  ASSERT_TRUE(s);
  // 05's grid: any phase (here 123.456789 ms after the epoch), exactly 10 ms steps, for 10 s.
  const int64_t t0 = kEpoch + 123'456'789;
  s->OnGap(t0);
  Feed(*s, t0, 1000, Sine(1000, 0.2));
  ASSERT_EQ(audio.packets.size(), 500u);
  for (std::size_t j = 0; j < audio.packets.size(); j++) {
    EXPECT_EQ(audio.packets[j].ts, t0 + static_cast<int64_t>(j) * kFrameNs - kLookaheadNs) << j;
    if (j > 0) {
      EXPECT_EQ(audio.packets[j].ts - audio.packets[j - 1].ts, 20'000'000) << j;
    }
    EXPECT_EQ(audio.packets[j].flags, j == 0 ? uint32_t{IM_PKT_REINIT} : 0u) << j;
  }
  // The rounding 01's publisher applies, rtpTicks((Δns * 48000 + 5e8) / 1e9), advances by exactly 960 per packet.
  for (std::size_t j = 1; j < audio.packets.size(); j++) {
    const int64_t d = audio.packets[j].ts - audio.packets[0].ts;
    EXPECT_EQ((d * 48000 + 500'000'000) / kSecond, static_cast<int64_t>(j) * 960);
  }
  EXPECT_EQ(s->stats().gaps, 1u);
}

TEST(OpusStage, ReinitOnFirstPacketAndAfterGap) {
  Recorder audio;
  auto s = MakeStage({.audio = &audio});
  ASSERT_TRUE(s);
  const int64_t t0 = kLookaheadNs;
  s->OnGap(t0);
  Feed(*s, t0, 5, Sine(440, 0.3)); // 2 packets and a half frame
  ASSERT_EQ(audio.packets.size(), 2u);
  EXPECT_EQ(audio.packets[0].flags, uint32_t{IM_PKT_REINIT});
  EXPECT_EQ(audio.packets[1].flags, 0u);

  // A new grid at any time step: the half frame is dropped, the next packet starts with the new grid's first chunk.
  const int64_t t1 = t0 + 5 * kChunkNs + 777'777'777;
  s->OnGap(t1);
  Feed(*s, t1, 4, Sine(440, 0.3));
  ASSERT_EQ(audio.packets.size(), 4u);
  EXPECT_EQ(audio.packets[2].flags, uint32_t{IM_PKT_REINIT});
  EXPECT_EQ(audio.packets[2].ts, t1 - kLookaheadNs);
  EXPECT_EQ(audio.packets[3].flags, 0u);
  EXPECT_EQ(audio.packets[3].ts, t1 - kLookaheadNs + kFrameNs);
  EXPECT_EQ(s->stats().gaps, 2u);

  // A chunk off the 10 ms grid without OnGap (05 never sends one) is taken as a new grid too.
  const int64_t t2 = t1 + 4 * kChunkNs + 5'000'000;
  Feed(*s, t2, 2, Sine(440, 0.3));
  ASSERT_EQ(audio.packets.size(), 5u);
  EXPECT_EQ(audio.packets[4].flags, uint32_t{IM_PKT_REINIT});
  EXPECT_EQ(audio.packets[4].ts, t2 - kLookaheadNs);
  EXPECT_EQ(s->stats().gaps, 3u);

  // Without an OnGap first, the very first packet is still REINIT.
  Recorder audio2;
  auto s2 = MakeStage({.audio = &audio2});
  ASSERT_TRUE(s2);
  Feed(*s2, kLookaheadNs, 4, Silence);
  ASSERT_EQ(audio2.packets.size(), 2u);
  EXPECT_EQ(audio2.packets[0].flags, uint32_t{IM_PKT_REINIT});
  EXPECT_EQ(audio2.packets[1].flags, 0u);
}

TEST(OpusStage, PacketTimesNeverGoBack) {
  Recorder audio;
  auto s = MakeStage({.audio = &audio});
  ASSERT_TRUE(s);
  const int64_t t0 = kSecond;
  Feed(*s, t0, 4, Silence);
  ASSERT_EQ(audio.packets.size(), 2u);
  // A new grid that would start 10 ms before the last chunk: its chunks are dropped until a frame would be stamped at
  // least 20 ms after the previous packet.
  const int64_t t1 = t0 + 2 * kChunkNs;
  s->OnGap(t1);
  Feed(*s, t1, 6, Silence);
  ASSERT_GE(audio.packets.size(), 3u);
  for (std::size_t j = 1; j < audio.packets.size(); j++) {
    EXPECT_GE(audio.packets[j].ts - audio.packets[j - 1].ts, kFrameNs) << j;
  }
  EXPECT_EQ(audio.packets[2].flags, uint32_t{IM_PKT_REINIT});
  EXPECT_EQ(audio.packets[2].ts, t1 + 2 * kChunkNs - kLookaheadNs);
  EXPECT_EQ(s->stats().early_chunks, 2u);
}

TEST(OpusStage, DiscontinuityAfterARingDropKeepsTheGrid) {
  Recorder audio;
  audio.refuse = {3, 4}; // the third and fourth packets meet a full ring
  auto s = MakeStage({.audio = &audio});
  ASSERT_TRUE(s);
  Feed(*s, kLookaheadNs, 20, Sine(440, 0.3));
  ASSERT_EQ(audio.packets.size(), 8u);
  EXPECT_EQ(audio.packets[0].flags, uint32_t{IM_PKT_REINIT});
  EXPECT_EQ(audio.packets[1].flags, 0u);
  EXPECT_EQ(audio.packets[2].flags, uint32_t{IM_PKT_DISCONTINUITY}); // no REINIT: the grid is unchanged
  EXPECT_EQ(audio.packets[2].ts - audio.packets[1].ts, 3 * kFrameNs);
  EXPECT_EQ(audio.packets[3].flags, 0u);
  EXPECT_EQ(s->stats().drops, 2u);
  EXPECT_EQ(s->stats().packets, 8u);
  EXPECT_EQ(s->stats().gaps, 0u);

  // A flag stays pending until a packet carrying it got in: a refused first packet leaves REINIT for the next one.
  Recorder audio2;
  audio2.refuse = {1};
  auto s2 = MakeStage({.audio = &audio2});
  ASSERT_TRUE(s2);
  Feed(*s2, kLookaheadNs, 6, Silence);
  ASSERT_EQ(audio2.packets.size(), 2u);
  EXPECT_EQ(audio2.packets[0].flags, uint32_t{IM_PKT_REINIT | IM_PKT_DISCONTINUITY});
  EXPECT_EQ(audio2.packets[0].ts, kFrameNs);
  EXPECT_EQ(audio2.packets[1].flags, 0u);
}

TEST(OpusStage, DropsChunksBeforeEpoch) {
  // 05 stamps chunks L0 = 50 ms in the past from the first one on, so the first chunks are older than epoch_ns.
  for (const int64_t phase : {int64_t{0}, int64_t{2'700'001}, int64_t{6'500'000}, int64_t{9'999'999}}) {
    SCOPED_TRACE(phase);
    Recorder audio, pcm;
    constexpr int64_t kEpoch = 1000 * kSecond;
    auto s = MakeStage({.epoch_ns = kEpoch, .flags = IM_AUDIOF_TAP_PCM, .audio = &audio, .pcm = &pcm});
    ASSERT_TRUE(s);
    const int64_t t0 = kEpoch - 50'000'000 - phase;
    s->OnGap(t0);
    Feed(*s, t0, 100, Sine(440, 0.3));
    ASSERT_FALSE(audio.packets.empty());
    ASSERT_FALSE(pcm.packets.empty());
    // The first packet is the first full frame stamped at or after epoch_ns, and it is REINIT.
    int64_t first_chunk = t0;
    while (first_chunk - kLookaheadNs < kEpoch) {
      first_chunk += kChunkNs;
    }
    EXPECT_EQ(audio.packets[0].ts, first_chunk - kLookaheadNs);
    EXPECT_GE(audio.packets[0].ts, kEpoch);
    EXPECT_LT(audio.packets[0].ts, kEpoch + kChunkNs);
    EXPECT_EQ(audio.packets[0].flags, uint32_t{IM_PKT_REINIT});
    EXPECT_EQ(s->stats().early_chunks, static_cast<uint64_t>((first_chunk - t0) / kChunkNs));
    // The tap drops chunks stamped before epoch_ns: its first packet is the first chunk at or after it.
    int64_t first_pcm = t0;
    while (first_pcm < kEpoch) {
      first_pcm += kChunkNs;
    }
    EXPECT_EQ(pcm.packets[0].ts, first_pcm);
    EXPECT_EQ(pcm.packets[0].flags, uint32_t{IM_PKT_REINIT});
    for (const auto &p : audio.packets) {
      EXPECT_GE(p.ts, kEpoch);
    }
    for (const auto &p : pcm.packets) {
      EXPECT_GE(p.ts, kEpoch);
    }
  }
}

// ------------------------------------------------------------------------------------------------- the audio

TEST(OpusStage, DecodedToneAt128kbps) {
  Recorder audio;
  auto s = MakeStage({.bitrate_bps = 128000, .audio = &audio});
  ASSERT_TRUE(s);
  constexpr int kChunks = 200; // 2 s
  const Signal tone = Sine(1000, 0.5);
  Feed(*s, kLookaheadNs, kChunks, tone);
  ASSERT_EQ(audio.packets.size(), static_cast<std::size_t>(kChunks / 2));
  const std::vector<float> out = DecodeAll(audio.packets);
  ASSERT_EQ(out.size(), std::size_t{kChunks} * kChunkSamples);
  // Decoded sample n is input sample n - lookahead; skip the first 100 ms (the encoder's start).
  const int64_t la = s->lookahead_samples();
  double sig = 0, err = 0, dec = 0;
  for (int64_t n = 4800; n < int64_t{kChunks} * kChunkFrames; n++) {
    for (int ch = 0; ch < 2; ch++) {
      const double ref = tone(n - la, ch);
      const double got = out[static_cast<std::size_t>(2 * n + ch)];
      sig += ref * ref;
      err += (got - ref) * (got - ref);
      dec += got * got;
    }
  }
  const double snr = 10 * std::log10(sig / err);
  const double level = 10 * std::log10(dec / sig);
  RecordProperty("snr_db", std::to_string(snr));
  EXPECT_GE(snr, 30.0);
  EXPECT_NEAR(level, 0.0, 1.0); // 05 §19.1: the round trip keeps the level within ±1 dB
}

TEST(OpusStage, SilenceKeepsFlowingAt50PacketsPerSecond) {
  Recorder audio;
  auto s = MakeStage({.audio = &audio});
  ASSERT_TRUE(s);
  // Sound, then a muted bus (zeros) for 10 s, then sound again: one packet every 20 ms throughout (DTX off).
  const int64_t t0 = kLookaheadNs;
  Feed(*s, t0, 100, Sine(440, 0.3));
  Feed(*s, t0 + 100 * kChunkNs, 1000, Silence);
  Feed(*s, t0 + 1100 * kChunkNs, 100, Sine(440, 0.3), 1100 * 480);
  ASSERT_EQ(audio.packets.size(), 600u);
  std::size_t silent_bytes = 0;
  for (std::size_t j = 0; j < audio.packets.size(); j++) {
    EXPECT_EQ(audio.packets[j].ts, static_cast<int64_t>(j) * kFrameNs);
    EXPECT_EQ(audio.packets[j].flags, j == 0 ? uint32_t{IM_PKT_REINIT} : 0u);
    EXPECT_EQ(audio.packets[j].duration_us, 20000u);
    // libopus's 3 Hz DC filter (APPLICATION_AUDIO) lets the sound's tail fade for about half a second; from then on
    // the encoder sees digital silence.
    if (j >= 110 && j < 540) {
      silent_bytes = std::max(silent_bytes, audio.packets[j].data.size());
    }
  }
  EXPECT_LE(silent_bytes, 16u) << "digital silence costs a few bytes per packet";
  EXPECT_EQ(s->stats().packets, 600u);
  const std::vector<float> out = DecodeAll(audio.packets);
  for (std::size_t n = 110 * 960; n < 540 * 960; n++) {
    ASSERT_LT(std::fabs(out[2 * n]), 1e-6f) << n;
  }
  // And it decodes as sound again afterwards.
  EXPECT_NEAR(ToneAmplitude(out, 560 * 960, 960, 440), 0.3, 0.03);
}

TEST(OpusStage, PcmTapFlowsWithOpus) {
  Recorder audio, pcm;
  auto s = MakeStage({.flags = IM_AUDIOF_TAP_PCM, .audio = &audio, .pcm = &pcm});
  ASSERT_TRUE(s);
  EXPECT_TRUE(s->tap_pcm());
  const int64_t t0 = 5 * kSecond;
  const Signal sig = Sine(1000, 0.25);
  Feed(*s, t0, 100, sig);
  ASSERT_EQ(audio.packets.size(), 50u);
  ASSERT_EQ(pcm.packets.size(), 100u);
  for (std::size_t k = 0; k < pcm.packets.size(); k++) {
    const auto &p = pcm.packets[k];
    EXPECT_EQ(p.kind, uint32_t{IM_PACKET_PCM});
    EXPECT_EQ(p.data.size(), 3840u);
    EXPECT_EQ(p.duration_us, 10000u);
    EXPECT_EQ(p.ts, t0 + static_cast<int64_t>(k) * kChunkNs); // the chunk's time, no lookahead
    EXPECT_EQ(p.flags, k == 0 ? uint32_t{IM_PKT_REINIT} : 0u);
    float f[kChunkSamples];
    std::memcpy(f, p.data.data(), sizeof f);
    for (uint32_t i = 0; i < kChunkFrames; i++) {
      ASSERT_EQ(f[2 * i], sig(static_cast<int64_t>(k * kChunkFrames + i), 0));
    }
  }
  // Each Opus packet's first chunk is a tap packet, lookahead later.
  for (std::size_t j = 0; j < audio.packets.size(); j++) {
    EXPECT_EQ(pcm.packets[2 * j].ts - audio.packets[j].ts, kLookaheadNs);
  }
  EXPECT_EQ(s->stats().pcm_packets, 100u);

  // A full PCM ring: DISCONTINUITY on the next PCM packet; the Opus stream doesn't notice.
  pcm.refuse = {pcm.calls + 2};
  Feed(*s, t0 + 100 * kChunkNs, 10, sig, 100 * 480);
  ASSERT_EQ(pcm.packets.size(), 109u);
  EXPECT_EQ(pcm.packets[101].flags, uint32_t{IM_PKT_DISCONTINUITY});
  EXPECT_EQ(pcm.packets[101].ts - pcm.packets[100].ts, 2 * kChunkNs);
  EXPECT_EQ(s->stats().pcm_drops, 1u);
  ASSERT_EQ(audio.packets.size(), 55u);
  for (std::size_t j = 1; j < audio.packets.size(); j++) {
    EXPECT_EQ(audio.packets[j].flags, 0u);
  }
  // A new grid marks both streams.
  const int64_t t1 = t0 + 2 * kSecond;
  s->OnGap(t1);
  Feed(*s, t1, 2, sig);
  EXPECT_EQ(pcm.packets[109].flags, uint32_t{IM_PKT_REINIT});
  EXPECT_EQ(audio.packets[55].flags, uint32_t{IM_PKT_REINIT});
}

TEST(OpusStage, LiveBitrateChange) {
  Recorder audio;
  auto s = MakeStage({.audio = &audio});
  ASSERT_TRUE(s);
  const Signal noise = Noise(0.5f, 1);
  const int64_t t0 = kLookaheadNs;
  Feed(*s, t0, 200, noise); // 2 s at 128 kbit/s
  EXPECT_EQ(s->SetBitrate(31999), IM_ERR_INVALID_ARG);
  EXPECT_EQ(s->SetBitrate(510001), IM_ERR_INVALID_ARG);
  EXPECT_EQ(s->SetBitrate(0), IM_ERR_INVALID_ARG);
  EXPECT_EQ(s->stats().bitrate_bps, 128000u);
  ASSERT_EQ(s->SetBitrate(256000), IM_OK);
  EXPECT_EQ(s->stats().bitrate_bps, 256000u);
  Feed(*s, t0 + 200 * kChunkNs, 200, noise, 200 * 480);
  ASSERT_EQ(audio.packets.size(), 200u);
  im::audio::OpusSettings got;
  ASSERT_EQ(s->ReadSettings(&got), IM_OK);
  EXPECT_EQ(got.bitrate, 256000);
  auto bytes = [&](std::size_t from, std::size_t to) {
    std::size_t n = 0;
    for (std::size_t j = from; j < to; j++) {
      n += audio.packets[j].data.size();
    }
    return static_cast<double>(n * 8) / (static_cast<double>(to - from) * 0.02); // bit/s
  };
  const double before = bytes(50, 100), after = bytes(150, 200);
  RecordProperty("bps_before", std::to_string(before));
  RecordProperty("bps_after", std::to_string(after));
  EXPECT_NEAR(before, 128000, 128000 * 0.25);
  EXPECT_NEAR(after, 256000, 256000 * 0.25);
  // Live: no reset, no new grid.
  for (std::size_t j = 1; j < audio.packets.size(); j++) {
    EXPECT_EQ(audio.packets[j].flags, 0u) << j;
    EXPECT_EQ(audio.packets[j].ts - audio.packets[j - 1].ts, kFrameNs);
  }
}

TEST(OpusStage, ConcurrentControlIsSafe) {
  // The mixer thread encodes while another thread changes the bitrate and reads the stats (TSan in host-tsan).
  Recorder audio, pcm;
  auto s = MakeStage({.flags = IM_AUDIOF_TAP_PCM, .audio = &audio, .pcm = &pcm});
  ASSERT_TRUE(s);
  std::atomic<bool> done{false};
  std::thread control([&] {
    uint32_t bps = 64000;
    while (!done.load()) {
      EXPECT_EQ(s->SetBitrate(bps), IM_OK);
      bps = bps == 64000 ? 256000 : 64000;
      (void)s->stats();
      std::this_thread::yield();
    }
  });
  std::thread mixer([&] { Feed(*s, kLookaheadNs, 300, Noise(0.3f, 2)); });
  mixer.join();
  done.store(true);
  control.join();
  EXPECT_EQ(audio.packets.size(), 150u);
  EXPECT_EQ(pcm.packets.size(), 300u);
  for (std::size_t j = 1; j < audio.packets.size(); j++) {
    EXPECT_EQ(audio.packets[j].flags, 0u);
  }
}

// ----------------------------------------------------------------------------------- the test tone's Marker

TEST(OpusStage, TestToneCarriesM1sMarker) {
  constexpr int64_t kEpoch = 42 * kSecond + 17;
  Recorder plain_out, marked_out;
  auto plain = MakeStage({.epoch_ns = kEpoch, .audio = &plain_out});
  auto marked = MakeStage({.epoch_ns = kEpoch, .audio = &marked_out, .marker = true});
  ASSERT_TRUE(plain && marked);

  // Both stages get the same tone, from the source the Windows test-tone thread runs.
  struct Both final : im::audio::PcmSink {
    OpusStage *a, *b;
    void OnChunk(const float *f, int64_t t) noexcept override {
      a->OnChunk(f, t);
      b->OnChunk(f, t);
    }
    void OnGap(int64_t t) noexcept override {
      a->OnGap(t);
      b->OnGap(t);
    }
  } both;
  both.a = plain.get();
  both.b = marked.get();
  im::audio::TestToneSource src(kEpoch, kEpoch + marked->lookahead_ns(), &both);
  for (int64_t now = kEpoch; now <= kEpoch + 3 * kSecond + 16'500'000; now += 1'000'000) {
    (void)src.Pump(now);
  }
  ASSERT_EQ(plain_out.packets.size(), 150u);
  ASSERT_EQ(marked_out.packets.size(), 150u);
  for (std::size_t j = 0; j < 150; j++) {
    SCOPED_TRACE(j);
    const auto &p = plain_out.packets[j];
    const auto &m = marked_out.packets[j];
    // Stamped like M1's fake audio: epoch_ns + j * 20 ms, the first one REINIT.
    EXPECT_EQ(m.ts, kEpoch + static_cast<int64_t>(j) * kFrameNs);
    EXPECT_EQ(m.ts, p.ts);
    EXPECT_EQ(m.flags, p.flags);
    const Marker want{.beep = j % 50 == 0, .frame = static_cast<uint32_t>(j), .capture_ns = m.ts - kEpoch};
    std::vector<uint8_t> expect(im::audio::kMaxPacketBytes);
    const std::size_t n = im::audio::WriteWithMarker(p.data, want, expect);
    ASSERT_GT(n, 0u);
    expect.resize(n);
    EXPECT_EQ(Hex(m.data), Hex(expect));
    ASSERT_LE(m.data.size(), im::audio::kMaxPacketBytes);
    Marker got;
    ASSERT_TRUE(im::audio::ParseAudioMarker(m.data, &got));
    EXPECT_EQ(got, want);
    EXPECT_FALSE(im::audio::ParseAudioMarker(p.data, &got)) << "plain Opus carries no Marker";
    EXPECT_EQ((m.data[0] & 3), 3); // code 3
  }
  // Decoders ignore the padding: both streams decode to the same samples.
  EXPECT_EQ(DecodeAll(plain_out.packets), DecodeAll(marked_out.packets));
}

TEST(OpusStage, TestToneBeepsStartOnPacketBoundaries) {
  constexpr int64_t kEpoch = 3 * kSecond;
  Recorder audio;
  auto s = MakeStage({.epoch_ns = kEpoch, .audio = &audio});
  ASSERT_TRUE(s);
  im::audio::TestToneSource src(kEpoch, kEpoch + s->lookahead_ns(), s.get());
  for (int64_t now = kEpoch; now <= kEpoch + 2 * kSecond + 20'000'000; now += 2'000'000) {
    (void)src.Pump(now);
  }
  ASSERT_GE(audio.packets.size(), 100u);
  EXPECT_EQ(audio.packets[0].ts, kEpoch);
  EXPECT_EQ(audio.packets[0].flags, uint32_t{IM_PKT_REINIT});
  // Decoded packet j holds the tone from epoch_ns + j * 20 ms on: the beep (1 kHz) fills packets 0-4 and 50-54 of
  // each second and none of the others.
  const std::vector<float> out = DecodeAll(audio.packets);
  double beep_min = 1, quiet_max = 0;
  for (std::size_t j = 1; j < 100; j++) {
    SCOPED_TRACE(j);
    const double a = ToneAmplitude(out, j * 960, 960, 1000);
    if (j % 50 < 5) {
      EXPECT_GT(a, 0.25); // 0.32 with the 1 ms edges
      beep_min = std::min(beep_min, a);
    } else {
      EXPECT_LT(a, 0.02); // packets 49 and 5 included: no pre- or post-echo across the boundary
      quiet_max = std::max(quiet_max, a);
    }
    EXPECT_NEAR(ToneAmplitude(out, j * 960, 960, 440), 0.1, 0.02); // the bed
  }
  RecordProperty("beep_min", std::to_string(beep_min));
  RecordProperty("quiet_max", std::to_string(quiet_max));
}

TEST(OpusStage, TestToneStaysOnThe20msGridAfterAStall) {
  // The source's skip after a long stall (a suspended PC, a loaded CI runner) starts a new grid on a frame boundary,
  // also when the stall began halfway through a frame: every packet stays on epoch_ns + k * 20 ms, so the beeps after
  // it keep their capture instants (k * 1 s), which the A/V checks pair with the video's flashes.
  constexpr int64_t kEpoch = 7 * kSecond;
  Recorder audio;
  auto s = MakeStage({.epoch_ns = kEpoch, .audio = &audio, .marker = true});
  ASSERT_TRUE(s);
  const int64_t first = kEpoch + s->lookahead_ns();
  im::audio::TestToneSource src(kEpoch, first, s.get());
  (void)src.Pump(first + kChunkNs);     // chunk 0
  (void)src.Pump(first + 3 * kChunkNs); // chunks 1 and 2: packet 0 and the first half of packet 1
  ASSERT_EQ(audio.packets.size(), 1u);
  // A 10 s stall, then the thread runs on time again for 2 s.
  int64_t now = first + 1016 * kChunkNs + 3'000'000;
  (void)src.Pump(now);
  ASSERT_EQ(src.skips(), 1u);
  for (const int64_t end = now + 2 * kSecond; now < end; now += 1'000'000) {
    (void)src.Pump(now);
  }
  EXPECT_EQ(src.skips(), 1u);
  ASSERT_EQ(audio.packets.size(), 102u); // packet 0, then 10.14 s to 12.14 s
  EXPECT_EQ(audio.packets[0].ts, kEpoch);
  EXPECT_EQ(audio.packets[1].ts, kEpoch + 10'140'000'000);
  EXPECT_EQ(audio.packets[1].flags, uint32_t{IM_PKT_REINIT}) << "the new grid";
  int beeps = 0;
  for (std::size_t j = 0; j < audio.packets.size(); j++) {
    SCOPED_TRACE(j);
    const Recorded &p = audio.packets[j];
    EXPECT_EQ((p.ts - kEpoch) % kFrameNs, 0) << "on epoch_ns + k * 20 ms";
    if (j >= 2) {
      EXPECT_EQ(p.ts, audio.packets[j - 1].ts + kFrameNs);
      EXPECT_EQ(p.flags, 0u);
    }
    Marker m;
    ASSERT_TRUE(im::audio::ParseAudioMarker(p.data, &m));
    EXPECT_EQ(m.capture_ns, p.ts - kEpoch);
    if (m.capture_ns % kSecond == 0) {
      EXPECT_TRUE(m.beep) << "the beep starts with this packet";
      beeps++;
    }
  }
  EXPECT_EQ(beeps, 3); // 0 s, 11 s and 12 s
}

TEST(OpusStage, MarkerGoldenBytes) {
  // Each "packet" line: frame, capture ns, beep, an Opus packet and the bytes the test tone's framing makes of it
  // with that Marker, which M1's fake.ParseAudioMarker reads (marker_golden_test.go checks the same file in Go).
  const std::string path = std::string(IM_TESTDATA_DIR) + "/tone_marker_golden.txt";
  std::ifstream f(path);
  ASSERT_TRUE(f) << "cannot read " << path;
  std::string line;
  int lines = 0;
  while (std::getline(f, line)) {
    if (!line.empty() && line.back() == '\r') {
      line.pop_back();
    }
    if (line.empty() || line[0] == '#') {
      continue;
    }
    SCOPED_TRACE(line);
    std::istringstream in(line);
    std::string kind, input_hex, framed_hex;
    uint32_t frame = 0;
    int64_t capture = 0;
    int beep = 0;
    in >> kind >> frame >> capture >> beep >> input_hex >> framed_hex;
    ASSERT_EQ(kind, "packet");
    ASSERT_FALSE(in.fail());
    const Marker m{.beep = beep != 0, .frame = frame, .capture_ns = capture};
    const std::vector<uint8_t> input = Unhex(input_hex);
    std::vector<uint8_t> framed(input.size() + 64);
    const std::size_t n = im::audio::WriteWithMarker(input, m, framed);
    ASSERT_GT(n, 0u);
    framed.resize(n);
    EXPECT_EQ(Hex(framed), framed_hex);
    EXPECT_EQ(n, im::audio::PaddedSize(input, im::audio::kMarkerSize));
    Marker got;
    ASSERT_TRUE(im::audio::ParseAudioMarker(framed, &got));
    EXPECT_EQ(got, m);
    // Same frames, padding = the Marker.
    im::audio::OpusFrames a, b;
    ASSERT_TRUE(im::audio::ParseOpusPacket(input, &a));
    ASSERT_TRUE(im::audio::ParseOpusPacket(framed, &b));
    EXPECT_EQ(b.toc, static_cast<uint8_t>(a.toc | 3));
    ASSERT_EQ(a.count, b.count);
    for (uint32_t i = 0; i < a.count; i++) {
      EXPECT_EQ(Hex(a.frames[i]), Hex(b.frames[i])) << i;
    }
    EXPECT_EQ(b.padding, im::audio::kMarkerSize);
    lines++;
  }
  EXPECT_GE(lines, 6);
}

// --------------------------------------------------------------------------------------------- Opus framing

TEST(OpusPacket, MarkerRoundTrip) {
  const Marker m{.rid = 'f',
                 .keyframe = true,
                 .flash = true,
                 .beep = false,
                 .frame = 0x01020304,
                 .capture_ns = 0x0102030405060708};
  uint8_t b[im::audio::kMarkerSize];
  im::audio::EncodeMarker(m, b);
  EXPECT_EQ(Hex(b), "4953484e6603010203040102030405060708");
  Marker got;
  ASSERT_TRUE(im::audio::DecodeMarker(b, &got));
  EXPECT_EQ(got, m);
  b[0] = 'X';
  EXPECT_FALSE(im::audio::DecodeMarker(b, &got));
  EXPECT_FALSE(im::audio::DecodeMarker(std::span<const uint8_t>(b, 17), &got));
}

TEST(OpusPacket, ParseChecksRfc6716Framing) {
  im::audio::OpusFrames f;
  auto parse = [&](std::vector<uint8_t> b) { return im::audio::ParseOpusPacket(b, &f); };
  EXPECT_FALSE(parse({}));              // R1
  EXPECT_TRUE(parse({0xfc}));           // code 0, an empty frame
  EXPECT_FALSE(parse({0xfd, 1, 2, 3})); // R3: code 1 with an odd payload
  EXPECT_TRUE(parse({0xfd, 1, 2, 3, 4}));
  EXPECT_EQ(f.count, 2u);
  EXPECT_FALSE(parse({0xfe, 5, 1, 2})); // R4: code 2's first frame longer than the packet
  EXPECT_TRUE(parse({0xfe, 1, 9, 8, 7}));
  EXPECT_EQ(f.frames[0].size(), 1u);
  EXPECT_EQ(f.frames[1].size(), 2u);
  EXPECT_FALSE(parse({0xff}));                            // R6: no frame count
  EXPECT_FALSE(parse({0xff, 0x00}));                      // R5: no frames
  EXPECT_FALSE(parse({0xff, 0x07, 1, 2, 3, 4, 5, 6, 7})); // R5: 7 x 20 ms > 120 ms
  EXPECT_FALSE(parse({0xff, 0x02, 1, 2, 3}));             // R6: CBR frames that don't divide evenly
  EXPECT_FALSE(parse({0xff, 0x41, 5, 1, 2}));             // R6, R7: padding longer than the packet
  EXPECT_FALSE(parse({0xff, 0x41}));                      // padding length runs out
  EXPECT_TRUE(parse({0xff, 0x41, 2, 7, 0, 0}));
  EXPECT_EQ(f.padding, 2u);
  EXPECT_EQ(f.frames[0].size(), 1u);
  EXPECT_FALSE(parse({0xff, 0x82, 9, 1})); // R7: a VBR length past the end
  EXPECT_TRUE(parse({0xff, 0x82, 1, 7, 8, 9}));
  EXPECT_EQ(f.frames[0].size(), 1u);
  EXPECT_EQ(f.frames[1].size(), 2u);
  std::vector<uint8_t> big{0xfc};
  big.resize(1 + 1276);
  EXPECT_FALSE(parse(big)); // R2: a frame above 1275 bytes
  big.resize(1 + 1275);
  EXPECT_TRUE(parse(big));
  // A padding length of 255 means 254 bytes and another length byte.
  std::vector<uint8_t> padded{0xff, 0x41, 255, 1, 0xaa};
  padded.resize(padded.size() + 255);
  EXPECT_TRUE(parse(padded));
  EXPECT_EQ(f.padding, 255u);
  EXPECT_EQ(f.frames[0].size(), 1u);
}

TEST(OpusPacket, WritePaddedHandlesLongPadding) {
  const std::vector<uint8_t> pkt{0xfc, 1, 2, 3};
  for (const std::size_t pad : {std::size_t{0}, std::size_t{18}, std::size_t{253}, std::size_t{254}, std::size_t{255},
                                std::size_t{508}, std::size_t{509}, std::size_t{1000}}) {
    SCOPED_TRACE(pad);
    std::vector<uint8_t> padding(pad, 0x5a);
    std::vector<uint8_t> out(im::audio::PaddedSize(pkt, pad));
    ASSERT_EQ(im::audio::WritePadded(pkt, padding, out), out.size());
    im::audio::OpusFrames f;
    ASSERT_TRUE(im::audio::ParseOpusPacket(out, &f));
    EXPECT_EQ(f.padding, pad);
    ASSERT_EQ(f.count, 1u);
    EXPECT_EQ(Hex(f.frames[0]), "010203");
    // Too small an output buffer writes nothing.
    std::vector<uint8_t> small(out.size() - 1);
    EXPECT_EQ(im::audio::WritePadded(pkt, padding, small), 0u);
  }
  std::vector<uint8_t> out(16);
  EXPECT_EQ(im::audio::WritePadded(std::vector<uint8_t>{}, {}, out), 0u); // R1
}

} // namespace
