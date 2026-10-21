// The struct layouts of the C ABI (docs/m2/04-windows-engine.md §5.4, §18.1): the sizes and offsets of §5.4 pinned at
// compile time, and the full im_abi_layout document (every field of every struct) compared with the golden file
// tests/testdata/abi_layout.json, which Go's layout_test.go and the Windows DLL test read too. A layout change without
// a minor ABI bump fails here.
#include <gtest/gtest.h>

#include <cstddef>
#include <fstream>
#include <set>
#include <sstream>
#include <string>

#include "im/abi/layout.h"
#include "isshoni_media.h"

namespace {

using im::abi::V1Size;

#define IM_SIZE(T, n)  static_assert(sizeof(T) == (n), #T " has the size of 04 §5.4")
#define IM_AT(T, f, n) static_assert(offsetof(T, f) == (n), #T "." #f " has the offset of 04 §5.4")

IM_SIZE(im_init_params, 32);
IM_AT(im_init_params, ffmpeg_dir, 16);
IM_AT(im_init_params, openh264_path, 24);
IM_SIZE(im_clock_sample, 32);
IM_SIZE(im_adapter_info, 160);
IM_AT(im_adapter_info, name, 24);
IM_SIZE(im_encoder_info, 104);
IM_AT(im_encoder_info, codec, 40);
IM_AT(im_encoder_info, reason, 72);
IM_SIZE(im_caps, 1568);
IM_AT(im_caps, adapters, 24);
IM_AT(im_caps, encoder_count, 664);
IM_AT(im_caps, encoders, 672);
IM_AT(im_caps, probe_ms, 1504);
IM_SIZE(im_pick_params, 24);
IM_AT(im_pick_params, parent_window, 8);
IM_SIZE(im_source_info, 440);
IM_AT(im_source_info, id, 8);
IM_AT(im_source_info, name, 72);
IM_AT(im_source_info, monitor_id, 328);
IM_AT(im_source_info, width, 392);
IM_AT(im_source_info, pid, 408);
IM_AT(im_source_info, window, 416);
IM_SIZE(im_rule, 32);
IM_AT(im_rule, value, 8);
IM_AT(im_rule, app_id, 16);
IM_AT(im_rule, name, 24);
IM_SIZE(im_exclusions, 24);
IM_AT(im_exclusions, rules, 8);
IM_AT(im_exclusions, rule_count, 16);
IM_AT(im_exclusions, shared_pid, 20);
IM_SIZE(im_audio_config, 32);
IM_AT(im_audio_config, mode, 0);
IM_AT(im_audio_config, bitrate_bps, 4);
IM_AT(im_audio_config, frame_ms, 8);
IM_AT(im_audio_config, flags, 12);
IM_SIZE(im_audio_level, 80);
IM_AT(im_audio_level, peak_db, 64);
IM_AT(im_audio_level, rms_db, 68);
IM_AT(im_audio_level, source, 72);
IM_SIZE(im_audio_levels, 5152);
IM_AT(im_audio_levels, qpc_ns, 8);
IM_AT(im_audio_levels, mix_peak_db, 16);
IM_AT(im_audio_levels, count, 24);
IM_AT(im_audio_levels, apps, 32);
IM_SIZE(im_layer_config, 40);
IM_SIZE(im_start_config, 224);
IM_AT(im_start_config, source_id, 8);
IM_AT(im_start_config, layers, 32);
IM_AT(im_start_config, audio, 152);
IM_AT(im_start_config, exclusions, 184);
IM_AT(im_start_config, reserved, 192);
IM_SIZE(im_layer_info, 40);
IM_SIZE(im_session_info, 184);
IM_AT(im_session_info, epoch_ns, 16);
IM_AT(im_session_info, layers, 32);
IM_SIZE(im_packet, 72);
IM_AT(im_packet, capture_ts_ns, 16);
IM_AT(im_packet, data, 24);
IM_AT(im_packet, size, 32);
IM_AT(im_packet, width, 40);
IM_AT(im_packet, profile, 44);
IM_AT(im_packet, event, 48);
IM_AT(im_packet, status, 52);

// In v1.0 every top-level struct is exactly its v1.0 size, with struct_size first.
template <class T> constexpr bool kV1Exact = sizeof(T) == V1Size<T>::value && offsetof(T, struct_size) == 0;
static_assert(kV1Exact<im_init_params> && kV1Exact<im_clock_sample> && kV1Exact<im_caps> && kV1Exact<im_pick_params> &&
              kV1Exact<im_source_info> && kV1Exact<im_exclusions> && kV1Exact<im_audio_levels> &&
              kV1Exact<im_start_config> && kV1Exact<im_session_info> && kV1Exact<im_packet>);

static_assert(IM_ABI_VERSION == 0x00010000u && IM_ABI_MINOR == 0u, "M2 ships v1.0");

std::string ReadFile(const std::string &path) {
  std::ifstream in(path, std::ios::binary);
  std::stringstream ss;
  ss << in.rdbuf();
  return ss.str();
}

// The golden file is indented for readable diffs (and git may check it out with CRLF on Windows): compare it without
// whitespace. No key or value in it contains a space.
std::string StripSpace(const std::string &s) {
  std::string out;
  for (char c : s) {
    if (c != ' ' && c != '\n' && c != '\r' && c != '\t') {
      out.push_back(c);
    }
  }
  return out;
}

TEST(AbiLayout, MatchesGoldenFile) {
  const std::string path = std::string(IM_TESTDATA_DIR) + "/abi_layout.json";
  const std::string golden = ReadFile(path);
  ASSERT_FALSE(golden.empty()) << "cannot read " << path;
  const std::string got = im::abi::LayoutJson();
  const std::string want = StripSpace(golden);
  if (got != want) {
    std::size_t i = 0;
    while (i < got.size() && i < want.size() && got[i] == want[i]) {
      ++i;
    }
    const std::size_t from = i < 80 ? 0 : i - 80;
    FAIL() << "im_abi_layout differs from " << path << " at byte " << i << "\n  got:  ..." << got.substr(from, 160)
           << "\n  want: ..." << want.substr(from, 160);
  }
}

TEST(AbiLayout, CoversEveryStructAndField) {
  const auto layouts = im::abi::Layouts();
  ASSERT_EQ(layouts.size(), 17u); // every struct of the header (04 §5.4's table)
  std::set<std::string_view> names;
  for (const auto &s : layouts) {
    EXPECT_TRUE(names.insert(s.name).second) << "duplicate " << s.name;
    ASSERT_FALSE(s.fields.empty()) << s.name;
    // Offsets rise strictly in declaration order and stay inside the struct.
    for (std::size_t k = 1; k < s.fields.size(); ++k) {
      EXPECT_GT(s.fields[k].offset, s.fields[k - 1].offset) << s.name << "." << s.fields[k].name;
    }
    EXPECT_LT(s.fields.back().offset, s.size) << s.name;
    EXPECT_EQ(s.fields.front().offset, 0u) << s.name;
  }
}

TEST(AbiLayout, DocumentShape) {
  const std::string doc = im::abi::LayoutJson();
  EXPECT_EQ(doc.rfind("{\"abi\":65536,\"structs\":{\"im_init_params\":{\"size\":32,\"fields\":{\"struct_size\":0,", 0),
            0u)
      << doc.substr(0, 120);
  EXPECT_NE(doc.find("\"im_packet\":{\"size\":72,\"fields\":{\"struct_size\":0,\"kind\":4,"), std::string::npos);
  EXPECT_EQ(doc.back(), '}');
}

} // namespace
