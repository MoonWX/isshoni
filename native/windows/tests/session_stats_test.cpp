// session_stats_test (docs/m2/04-windows-engine.md §5.6, E14; README WA52, §3 item 35): im_get_stats' document and
// the audio start check after IM_INIT_NO_AUDIO, through the C ABI.
//
// The document of a running session has every field of 04 §5.6, in its order and nesting: for each layer rid,
// encoder, codec, hw, width, height, active, fpsIn, fpsOut, bitrateBps, targetBps, keyframes, refreshes, reinits,
// drops {pacer, ring, stale}, encodeMs {p50, p95, p99}, latencyMs {p50, p95}, limit; capture {path, sourceWidth,
// sourceHeight, frames, interrupted}; audio {state, packets, drops, bitrateBps}; paused. The values are checked where
// the session fixes them (the fake's tokens, the sizes, the targets, the tone's state and bitrate, paused) and for
// their type elsewhere; an audio-only session has an empty layers array and no capture path.
//
// After im_init(IM_INIT_NO_AUDIO), im_start with IM_AUDIO_MODE_EXCLUDE_APPS is IM_ERR_BAD_STATE (audio-only and with
// video, and a pick token survives the refusal), while the test tone still starts (05 §4.3).
#include <gtest/gtest.h>

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

#include "isshoni_media.h"

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
#include "testhooks/testhooks.h"
#endif

namespace {

using std::chrono::milliseconds;
using Clock = std::chrono::steady_clock;

std::string LastError() {
  char buf[1100];
  const int32_t n = im_last_error(buf, static_cast<int32_t>(sizeof buf));
  return n < 0 ? std::string("<im_last_error failed>")
               : std::string(buf, std::min<std::size_t>(static_cast<std::size_t>(n), sizeof buf - 1));
}

// im_get_stats' document ("" when it fails).
std::string StatsOf(im_session s) {
  std::vector<char> buf(16384);
  const int32_t n = im_get_stats(s, buf.data(), static_cast<int32_t>(buf.size()));
  EXPECT_GT(n, 0) << LastError();
  return n > 0 ? std::string(buf.data(), static_cast<std::size_t>(std::min<int32_t>(n, 16383))) : std::string();
}

// Drains the session's packets for d.
void Drain(im_session s, milliseconds d) {
  const auto end = Clock::now() + d;
  while (Clock::now() < end) {
    im_packet p{};
    p.struct_size = sizeof p;
    const im_status st = im_next_packet(s, 50, &p);
    if (st != IM_OK && st != IM_TIMEOUT) {
      break;
    }
  }
}

// The document's structure as one string: every key with its path ("layers[].drops.pacer"), in order, each followed by
// the kind of its value (s string, n number, b bool, o object, a array). A minimal reader for the engine's own JSON:
// no whitespace, no escapes in keys.
struct Shape {
  std::vector<std::string> keys; // "path:kind"
  bool ok = true;
};

class Reader {
public:
  explicit Reader(const std::string &doc) : d_(doc) {}

  Shape Read() {
    Value("");
    if (i_ != d_.size()) {
      shape_.ok = false;
    }
    return shape_;
  }

private:
  char Peek() const { return i_ < d_.size() ? d_[i_] : '\0'; }

  bool Eat(char c) {
    if (Peek() != c) {
      shape_.ok = false;
      return false;
    }
    ++i_;
    return true;
  }

  std::string String() {
    std::string out;
    if (!Eat('"')) {
      return out;
    }
    while (i_ < d_.size() && d_[i_] != '"') {
      if (d_[i_] == '\\') {
        ++i_;
      }
      if (i_ < d_.size()) {
        out.push_back(d_[i_++]);
      }
    }
    Eat('"');
    return out;
  }

  // Reads one value; returns its kind.
  char Value(const std::string &path) {
    const char c = Peek();
    if (c == '{') {
      ++i_;
      bool first = true;
      while (shape_.ok && Peek() != '}') {
        if (!first && !Eat(',')) {
          break;
        }
        first = false;
        const std::string key = String();
        if (!Eat(':')) {
          break;
        }
        const std::string sub = path.empty() ? key : path + "." + key;
        const std::size_t at = shape_.keys.size();
        shape_.keys.push_back(sub);
        const char kind = Value(sub);
        shape_.keys[at] += std::string(":") + kind;
      }
      Eat('}');
      return 'o';
    }
    if (c == '[') {
      ++i_;
      bool first = true;
      while (shape_.ok && Peek() != ']') {
        if (!first && !Eat(',')) {
          break;
        }
        first = false;
        Value(path + "[]");
      }
      Eat(']');
      return 'a';
    }
    if (c == '"') {
      String();
      return 's';
    }
    if (d_.compare(i_, 4, "true") == 0) {
      i_ += 4;
      return 'b';
    }
    if (d_.compare(i_, 5, "false") == 0) {
      i_ += 5;
      return 'b';
    }
    const std::size_t start = i_;
    while (i_ < d_.size() && (std::strchr("-+.eE", d_[i_]) != nullptr || (d_[i_] >= '0' && d_[i_] <= '9'))) {
      ++i_;
    }
    if (i_ == start) {
      shape_.ok = false;
    }
    return 'n';
  }

  const std::string &d_;
  std::size_t i_ = 0;
  Shape shape_;
};

// 04 §5.6's keys of one layer, in order.
std::vector<std::string> LayerKeys() {
  return {"layers[].rid:s",           "layers[].encoder:s",      "layers[].codec:s",     "layers[].hw:b",
          "layers[].width:n",         "layers[].height:n",       "layers[].active:b",    "layers[].fpsIn:n",
          "layers[].fpsOut:n",        "layers[].bitrateBps:n",   "layers[].targetBps:n", "layers[].keyframes:n",
          "layers[].refreshes:n",     "layers[].reinits:n",      "layers[].drops:o",     "layers[].drops.pacer:n",
          "layers[].drops.ring:n",    "layers[].drops.stale:n",  "layers[].encodeMs:o",  "layers[].encodeMs.p50:n",
          "layers[].encodeMs.p95:n",  "layers[].encodeMs.p99:n", "layers[].latencyMs:o", "layers[].latencyMs.p50:n",
          "layers[].latencyMs.p95:n", "layers[].limit:s"};
}

// The whole document's keys for a session of `layers` layers.
std::vector<std::string> DocumentKeys(uint32_t layers) {
  std::vector<std::string> keys{"layers:a"};
  for (uint32_t l = 0; l < layers; ++l) {
    for (const std::string &k : LayerKeys()) {
      keys.push_back(k);
    }
  }
  for (const char *k : {"capture:o", "capture.path:s", "capture.sourceWidth:n", "capture.sourceHeight:n",
                        "capture.frames:n", "capture.interrupted:b", "audio:o", "audio.state:s", "audio.packets:n",
                        "audio.drops:n", "audio.bitrateBps:n", "paused:b"}) {
    keys.push_back(k);
  }
  return keys;
}

void ExpectShape(const std::string &doc, uint32_t layers) {
  const Shape got = Reader(doc).Read();
  EXPECT_TRUE(got.ok) << "not the engine's JSON: " << doc;
  EXPECT_EQ(got.keys, DocumentKeys(layers)) << doc;
}

// The number right after the first occurrence of key at or after `from` (-1 when there is none).
double NumberAfter(const std::string &doc, const std::string &key, std::size_t from = 0) {
  const std::size_t at = doc.find(key, from);
  return at == std::string::npos ? -1.0 : std::strtod(doc.c_str() + at + key.size(), nullptr);
}

bool Has(const std::string &doc, const std::string &part) { return doc.find(part) != std::string::npos; }

class Stats : public ::testing::Test {
protected:
  void SetUp() override {
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
    im::testhooks::Reset();
#endif
    Init(IM_INIT_NO_AUDIO); // CI runners have no audio device; the test tone needs none
  }
  void TearDown() override {
    im_shutdown();
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
    im::testhooks::Reset();
#endif
  }

  static void Init(uint32_t flags) {
    im_init_params p{};
    p.struct_size = sizeof p;
    p.abi_version = IM_ABI_VERSION;
    p.flags = flags;
    p.log_level = IM_LOG_DEBUG;
    ASSERT_EQ(im_init(&p), IM_OK) << LastError();
  }
};

im_start_config ToneOnly() {
  im_start_config c{};
  c.struct_size = sizeof c;
  c.audio.mode = IM_AUDIO_MODE_TEST_TONE;
  return c;
}

TEST_F(Stats, AudioOnlyDocumentHasEveryFieldAndNoLayers) {
  const im_start_config cfg = ToneOnly();
  im_session s = 0;
  ASSERT_EQ(im_start(&cfg, &s, nullptr), IM_OK) << LastError();
  Drain(s, milliseconds(600));
  const std::string doc = StatsOf(s);
  ExpectShape(doc, 0);
  EXPECT_TRUE(Has(doc, "{\"layers\":[],\"capture\":{\"path\":\"\",\"sourceWidth\":0,\"sourceHeight\":0,\"frames\":0,"
                       "\"interrupted\":false}"))
      << doc;
  EXPECT_TRUE(Has(doc, "\"audio\":{\"state\":\"ok\",")) << doc;
  EXPECT_TRUE(Has(doc, "\"bitrateBps\":128000}")) << doc;
  EXPECT_GE(NumberAfter(doc, "\"packets\":"), 20.0) << doc;
  EXPECT_TRUE(Has(doc, "\"paused\":false}")) << doc;
  EXPECT_EQ(im_stop(s), IM_OK);
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

im_layer_config Layer(const char *rid, uint32_t max_pixels, uint32_t fps, uint32_t bps) {
  im_layer_config l{};
  std::memcpy(l.rid, rid, std::strlen(rid) + 1);
  l.max_pixels = max_pixels;
  l.fps = fps;
  l.bitrate_bps = bps;
  return l;
}

// test:pattern (or a pick) through IM_ENC_FAKE: f 1080p60 at 4 Mbit/s, q 360p15 at 300 kbit/s, and the given audio.
im_start_config VideoConfig(uint32_t audio_mode, const char *source = "test:pattern") {
  im_start_config c{};
  c.struct_size = sizeof c;
  c.flags = IM_START_ALLOW_LISTED_SOURCE;
  c.source_id = source;
  c.profile = IM_PROFILE_HIGH;
  c.content_hint = IM_CONTENT_AUTO;
  c.encoder = IM_ENC_AUTO;
  c.layer_count = 2;
  c.layers[0] = Layer("f", 1920 * 1080, 60, 4'000'000);
  c.layers[1] = Layer("q", 640 * 360, 15, 300'000);
  c.audio.mode = audio_mode;
  return c;
}

TEST_F(Stats, DocumentHasEveryFieldOf04Section56) {
  const im_start_config cfg = VideoConfig(IM_AUDIO_MODE_TEST_TONE);
  im_session s = 0;
  ASSERT_EQ(im_start(&cfg, &s, nullptr), IM_OK) << LastError();
  Drain(s, milliseconds(2500)); // a full 2 s window
  const std::string doc = StatsOf(s);
  ExpectShape(doc, 2);

  const std::size_t q = doc.find("{\"rid\":\"q\"");
  ASSERT_NE(q, std::string::npos) << doc;
  EXPECT_TRUE(Has(doc, "{\"layers\":[{\"rid\":\"f\",\"encoder\":\"fake\",\"codec\":\"fake\",\"hw\":false,"
                       "\"width\":1920,\"height\":1080,\"active\":true,\"fpsIn\":"))
      << doc;
  EXPECT_TRUE(Has(doc, "{\"rid\":\"q\",\"encoder\":\"fake\",\"codec\":\"fake\",\"hw\":false,\"width\":640,"
                       "\"height\":360,\"active\":true,\"fpsIn\":"))
      << doc;
  EXPECT_EQ(NumberAfter(doc, "\"targetBps\":"), 4'000'000.0) << doc;
  EXPECT_EQ(NumberAfter(doc, "\"targetBps\":", q), 300'000.0) << doc;
  EXPECT_GT(NumberAfter(doc, "\"fpsIn\":"), 40.0) << "the 60 Hz pattern: " << doc;
  EXPECT_GT(NumberAfter(doc, "\"fpsOut\":"), 40.0) << doc;
  EXPECT_GT(NumberAfter(doc, "\"fpsOut\":", q), 8.0) << doc;
  EXPECT_GT(NumberAfter(doc, "\"bitrateBps\":"), 0.0) << doc;
  EXPECT_GE(NumberAfter(doc, "\"keyframes\":"), 1.0) << doc;
  EXPECT_EQ(NumberAfter(doc, "\"reinits\":"), 0.0) << "the first open is no re-init: " << doc;
  EXPECT_TRUE(Has(doc, "\"limit\":\"none\"}")) << doc;
  EXPECT_TRUE(Has(doc, "\"capture\":{\"path\":\"test\",\"sourceWidth\":1920,\"sourceHeight\":1080,\"frames\":")) << doc;
  EXPECT_GT(NumberAfter(doc, "\"frames\":"), 100.0) << doc;
  EXPECT_TRUE(Has(doc, "\"interrupted\":false}")) << doc;
  EXPECT_TRUE(Has(doc, "\"audio\":{\"state\":\"ok\",\"packets\":")) << doc;
  EXPECT_TRUE(Has(doc, "\"bitrateBps\":128000},\"paused\":false}")) << doc;

  // Paused: the document says so, and fpsIn keeps counting the captured frames (§5.6, §8.5).
  ASSERT_EQ(im_set_paused(s, 1), IM_OK) << LastError();
  Drain(s, milliseconds(2200));
  const std::string paused = StatsOf(s);
  ExpectShape(paused, 2);
  EXPECT_TRUE(Has(paused, ",\"paused\":true}")) << paused;
  EXPECT_GT(NumberAfter(paused, "\"fpsIn\":"), 40.0) << paused;
  EXPECT_LT(NumberAfter(paused, "\"fpsOut\":"), 10.0) << "only refreshes while paused: " << paused;
  ASSERT_EQ(im_set_paused(s, 0), IM_OK) << LastError();
  Drain(s, milliseconds(300));
  EXPECT_TRUE(Has(StatsOf(s), ",\"paused\":false}"));
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Stats, ExcludeAppsAfterInitNoAudioIsBadStateWhileTheToneStarts) {
  // Audio-only and with video: refused, and nothing was opened (a pick token stays valid).
  im_start_config audio{};
  audio.struct_size = sizeof audio;
  audio.audio.mode = IM_AUDIO_MODE_EXCLUDE_APPS;
  im_session s = 0;
  EXPECT_EQ(im_start(&audio, &s, nullptr), IM_ERR_BAD_STATE);
  EXPECT_EQ(s, 0u);
  EXPECT_NE(LastError().find("IM_INIT_NO_AUDIO"), std::string::npos) << LastError();

  const std::string pick = im::testhooks::FakePick(IM_SOURCE_WINDOW, 1280, 720);
  im_start_config video = VideoConfig(IM_AUDIO_MODE_EXCLUDE_APPS, pick.c_str());
  EXPECT_EQ(im_start(&video, &s, nullptr), IM_ERR_BAD_STATE);
  EXPECT_EQ(s, 0u);

  // The test tone is 04's own source: it still starts, and so does a video share without audio, on the same pick.
  video.audio.mode = IM_AUDIO_MODE_TEST_TONE;
  im_session_info info{};
  info.struct_size = sizeof info;
  ASSERT_EQ(im_start(&video, &s, &info), IM_OK) << "the refused start consumed no pick token: " << LastError();
  EXPECT_NE(info.flags & IM_SESS_AUDIO, 0u);
  EXPECT_EQ(info.audio_state, IM_AUDIO_STATE_OK);
  EXPECT_EQ(im_stop(s), IM_OK);
  const im_start_config tone = ToneOnly();
  ASSERT_EQ(im_start(&tone, &s, nullptr), IM_OK) << LastError();
  EXPECT_EQ(im_stop(s), IM_OK);
  const im_start_config off = VideoConfig(IM_AUDIO_MODE_OFF);
  ASSERT_EQ(im_start(&off, &s, nullptr), IM_OK) << LastError();
  EXPECT_EQ(im_stop(s), IM_OK);
}

#else

TEST(Stats, VideoNeedsTestHooks) { GTEST_SKIP() << "the video cases need an IM_TEST_HOOKS build (win-x64-ci)"; }

#endif // IM_TEST_HOOKS

} // namespace
