// The engine's log ring (docs/m2/04-windows-engine.md §5.6, §6.1): the NDJSON record format, whole lines only with
// the rest left queued, levels, drop counting when full, string limits, and concurrent producers (run under host-tsan
// too).
#include <gtest/gtest.h>

#include <atomic>
#include <map>
#include <string>
#include <thread>
#include <vector>

#include "im/log/ring.h"

namespace {

using im::log::Ring;

// 2026-10-21T12:00:00.000Z
constexpr int64_t kT0 = 1792584000LL * 1000000000LL;

std::string DrainAll(Ring &r, int32_t cap = 64 * 1024) {
  std::string buf(static_cast<std::size_t>(cap), '\0');
  const int32_t n = r.Drain(buf.data(), cap);
  EXPECT_GE(n, 0);
  EXPECT_EQ(buf[static_cast<std::size_t>(n)], '\0');
  buf.resize(static_cast<std::size_t>(n));
  return buf;
}

std::vector<std::string> Lines(const std::string &s) {
  std::vector<std::string> out;
  std::size_t start = 0;
  while (start < s.size()) {
    const std::size_t nl = s.find('\n', start);
    EXPECT_NE(nl, std::string::npos) << "every line ends with a newline";
    if (nl == std::string::npos) {
      break;
    }
    out.push_back(s.substr(start, nl - start));
    start = nl + 1;
  }
  return out;
}

TEST(LogRing, Timestamp) {
  EXPECT_EQ(im::log::FormatTimestamp(kT0), "2026-10-21T12:00:00.000Z");
  EXPECT_EQ(im::log::FormatTimestamp(0), "1970-01-01T00:00:00.000Z");
  EXPECT_EQ(im::log::FormatTimestamp(951782400LL * 1000000000LL + 123456789), "2000-02-29T00:00:00.123Z");
  EXPECT_EQ(im::log::FormatTimestamp(4102444799LL * 1000000000LL + 999999999), "2099-12-31T23:59:59.999Z");
  EXPECT_EQ(im::log::FormatTimestamp(-5), "1970-01-01T00:00:00.000Z");
}

TEST(LogRing, RecordFormat) {
  Ring r(8);
  ASSERT_TRUE(r.PushAt(kT0, IM_LOG_INFO, "encode", "encoder opened",
                       {{"layer", "f"}, {"encoder", "nvenc"}, {"w", 1920}, {"h", 1080u}}));
  ASSERT_TRUE(r.PushAt(kT0 + 1500000, IM_LOG_WARN, "capture", "frame late",
                       {{"ms", 17.5}, {"keyframe", true}, {"delta", int64_t{-3}}}));
  ASSERT_TRUE(r.PushAt(kT0, IM_LOG_ERROR, "engine", "no attrs"));
  EXPECT_EQ(
      DrainAll(r),
      "{\"t\":\"2026-10-21T12:00:00.000Z\",\"level\":\"info\",\"component\":\"encode\",\"msg\":\"encoder opened\","
      "\"attrs\":{\"layer\":\"f\",\"encoder\":\"nvenc\",\"w\":1920,\"h\":1080}}\n"
      "{\"t\":\"2026-10-21T12:00:00.001Z\",\"level\":\"warn\",\"component\":\"capture\",\"msg\":\"frame late\","
      "\"attrs\":{\"ms\":17.5,\"keyframe\":true,\"delta\":-3}}\n"
      "{\"t\":\"2026-10-21T12:00:00.000Z\",\"level\":\"error\",\"component\":\"engine\",\"msg\":\"no attrs\","
      "\"attrs\":{}}\n");
  EXPECT_EQ(DrainAll(r), "") << "drained: 0 = empty";
}

TEST(LogRing, StringsAreEscapedAndCut) {
  Ring r(8);
  r.PushAt(kT0, IM_LOG_INFO, "c", "quote \" and\nnewline \xF0\x9F\x8E\xAE \xED\xA0\x80", {{"k\"", "v\\"}});
  const auto lines = Lines(DrainAll(r));
  ASSERT_EQ(lines.size(), 1u);
  EXPECT_NE(lines[0].find("\"msg\":\"quote \\\" and\\nnewline \xF0\x9F\x8E\xAE \xEF\xBF\xBD\xEF\xBF\xBD\xEF\xBF\xBD\""),
            std::string::npos)
      << lines[0];
  EXPECT_NE(lines[0].find("\"attrs\":{\"k\\\"\":\"v\\\\\"}"), std::string::npos) << lines[0];

  r.PushAt(kT0, IM_LOG_INFO, std::string(40, 'c'), std::string(1000, 'm'),
           {{"a", 1}, {"b", 2}, {"c", 3}, {"d", 4}, {"e", 5}, {"f", 6}, {"g", 7}, {"h", 8}, {"i", 9}});
  const auto cut = Lines(DrainAll(r));
  ASSERT_EQ(cut.size(), 1u);
  EXPECT_NE(cut[0].find("\"component\":\"" + std::string(im::log::kMaxComponentBytes, 'c') + "\""), std::string::npos);
  EXPECT_NE(cut[0].find("\"msg\":\"" + std::string(im::log::kMaxMsgBytes, 'm') + "\""), std::string::npos);
  EXPECT_NE(cut[0].find("\"h\":8}"), std::string::npos) << "8 attributes kept" << cut[0];
  EXPECT_EQ(cut[0].find("\"i\":9"), std::string::npos) << "the ninth is left out";
}

TEST(LogRing, LongStringAttributeIsCutOnABoundary) {
  Ring r(4);
  r.PushAt(kT0, IM_LOG_INFO, "c", "m", {{"k", std::string(62, 'v') + "\xC3\xA9"}});
  const auto lines = Lines(DrainAll(r));
  ASSERT_EQ(lines.size(), 1u);
  EXPECT_NE(lines[0].find("\"k\":\"" + std::string(62, 'v') + "\"}"), std::string::npos) << lines[0];
}

TEST(LogRing, Levels) {
  Ring r(8);
  EXPECT_EQ(r.level(), IM_LOG_INFO) << "the default";
  EXPECT_FALSE(r.PushAt(kT0, IM_LOG_DEBUG, "c", "debug is off"));
  EXPECT_TRUE(r.PushAt(kT0, IM_LOG_INFO, "c", "info"));
  r.SetLevel(IM_LOG_ERROR);
  EXPECT_FALSE(r.PushAt(kT0, IM_LOG_WARN, "c", "warn is off"));
  EXPECT_TRUE(r.PushAt(kT0, IM_LOG_ERROR, "c", "error"));
  r.SetLevel(99);
  EXPECT_EQ(r.level(), IM_LOG_DEBUG) << "above debug means debug";
  EXPECT_TRUE(r.PushAt(kT0, 7, "c", "an unknown level is debug"));
  const auto lines = Lines(DrainAll(r));
  ASSERT_EQ(lines.size(), 3u);
  EXPECT_NE(lines[2].find("\"level\":\"debug\""), std::string::npos);
  EXPECT_EQ(r.pending_drops(), 0u) << "filtered records are not drops";
}

TEST(LogRing, ARecordThatDoesNotFitStaysQueued) {
  Ring r(64);
  // Each record is about 180 bytes: 50 of them exceed a buffer of kMaxLineBytes + 1 = 8193 bytes.
  for (int i = 0; i < 50; ++i) {
    r.PushAt(kT0, IM_LOG_INFO, "component", std::string(100, 'x'), {{"i", i}});
  }
  const int32_t cap = im::log::kMaxLineBytes + 1;
  std::string first(static_cast<std::size_t>(cap), '\0');
  const int32_t n1 = r.Drain(first.data(), cap);
  ASSERT_GT(n1, 0);
  ASSERT_LE(n1, cap - 1);
  const auto lines1 = Lines(first.substr(0, static_cast<std::size_t>(n1)));
  ASSERT_LT(lines1.size(), 50u) << "the buffer took only part of the queue";
  const auto lines2 = Lines(DrainAll(r));
  ASSERT_EQ(lines1.size() + lines2.size(), 50u) << "nothing lost";
  EXPECT_NE(lines2[0].find("\"i\":" + std::to_string(lines1.size()) + "}"), std::string::npos) << "in order";
}

TEST(LogRing, DrainArguments) {
  Ring r(4);
  char small[64];
  EXPECT_EQ(r.Drain(nullptr, 100000), IM_ERR_INVALID_ARG);
  EXPECT_EQ(r.Drain(small, static_cast<int32_t>(sizeof small)), IM_ERR_INVALID_ARG) << "a record could never fit";
  EXPECT_EQ(r.Drain(small, 0), IM_ERR_INVALID_ARG);
  EXPECT_EQ(r.Drain(small, -1), IM_ERR_INVALID_ARG);
  std::string ok(static_cast<std::size_t>(im::log::kMaxLineBytes + 1), 'x');
  EXPECT_EQ(r.Drain(ok.data(), im::log::kMaxLineBytes + 1), 0);
  EXPECT_EQ(ok[0], '\0');
}

TEST(LogRing, FullRingDropsAndCountsInTheNextRecord) {
  Ring r(4);
  for (int i = 0; i < 4; ++i) {
    EXPECT_TRUE(r.PushAt(kT0, IM_LOG_INFO, "c", "kept", {{"i", i}}));
  }
  for (int i = 0; i < 3; ++i) {
    EXPECT_FALSE(r.PushAt(kT0, IM_LOG_INFO, "c", "dropped"));
  }
  EXPECT_EQ(r.pending_drops(), 3u);
  auto lines = Lines(DrainAll(r));
  ASSERT_EQ(lines.size(), 4u);
  for (const auto &l : lines) {
    EXPECT_EQ(l.find("dropped"), std::string::npos) << l;
  }
  EXPECT_TRUE(r.PushAt(kT0, IM_LOG_INFO, "c", "after"));
  EXPECT_EQ(r.pending_drops(), 0u);
  lines = Lines(DrainAll(r));
  ASSERT_EQ(lines.size(), 1u);
  EXPECT_NE(lines[0].find("\"msg\":\"after\",\"dropped\":3,\"attrs\":{}"), std::string::npos) << lines[0];
}

TEST(LogRing, WrapsAroundManyTimes) {
  Ring r(4);
  for (int round = 0; round < 100; ++round) {
    for (int i = 0; i < 3; ++i) {
      ASSERT_TRUE(r.PushAt(kT0, IM_LOG_INFO, "c", "m", {{"round", round}, {"i", i}}));
    }
    const auto lines = Lines(DrainAll(r));
    ASSERT_EQ(lines.size(), 3u);
    EXPECT_NE(lines[2].find("\"round\":" + std::to_string(round) + ",\"i\":2"), std::string::npos);
  }
}

TEST(LogRing, TheDefaultRingAndHelpers) {
  Ring &d = im::log::Default();
  EXPECT_EQ(&d, &im::log::Default());
  EXPECT_EQ(d.capacity(), im::log::kCapacity);
  std::string sink(static_cast<std::size_t>(im::log::kMaxLineBytes + 1), '\0');
  while (d.Drain(sink.data(), static_cast<int32_t>(sink.size())) > 0) {
  }
  EXPECT_TRUE(im::log::Info("engine", "started", {{"abi", IM_ABI_VERSION}}));
  EXPECT_TRUE(im::log::Error("engine", "failed"));
  EXPECT_FALSE(im::log::Debug("engine", "debug is off by default"));
  const auto lines = Lines(DrainAll(d));
  ASSERT_EQ(lines.size(), 2u);
  EXPECT_NE(lines[0].find("\"component\":\"engine\",\"msg\":\"started\",\"attrs\":{\"abi\":65536}"), std::string::npos);
  EXPECT_EQ(lines[0].rfind("{\"t\":\"", 0), 0u);
}

// Producers on several threads and a drainer racing them: per-producer order holds, nothing is lost unless counted as
// dropped, and every line is well-formed. Clean under ThreadSanitizer (host-tsan).
TEST(LogRing, ConcurrentProducers) {
  Ring r(64);
  constexpr int kProducers = 4;
  constexpr int kPerProducer = 5000;
  std::atomic<int> done{0};
  std::map<int, int> next_seq;
  uint64_t dropped_seen = 0;
  int received = 0;

  std::vector<std::thread> producers;
  for (int p = 0; p < kProducers; ++p) {
    producers.emplace_back([&, p] {
      for (int i = 0; i < kPerProducer; ++i) {
        r.PushAt(kT0, IM_LOG_INFO, "p", "m", {{"p", p}, {"i", i}});
      }
      done.fetch_add(1);
    });
  }
  std::string buf(64 * 1024, '\0');
  auto consume = [&] {
    const int32_t n = r.Drain(buf.data(), static_cast<int32_t>(buf.size()));
    ASSERT_GE(n, 0);
    for (const auto &line : Lines(buf.substr(0, static_cast<std::size_t>(n)))) {
      ASSERT_EQ(line.back(), '}');
      const auto dpos = line.find("\"dropped\":");
      if (dpos != std::string::npos) {
        dropped_seen += std::stoull(line.substr(dpos + 10));
      }
      const auto ppos = line.find("\"p\":");
      const auto ipos = line.find(",\"i\":");
      ASSERT_NE(ppos, std::string::npos);
      ASSERT_NE(ipos, std::string::npos);
      const int p = std::stoi(line.substr(ppos + 4));
      const int i = std::stoi(line.substr(ipos + 5));
      EXPECT_GE(i, next_seq[p]) << "per-producer order";
      next_seq[p] = i + 1;
      ++received;
    }
  };
  while (done.load() < kProducers) {
    consume();
  }
  for (auto &t : producers) {
    t.join();
  }
  consume();
  consume();
  EXPECT_GE(received, 64) << "at least a ring's worth got through";
  EXPECT_EQ(static_cast<uint64_t>(received) + dropped_seen + r.pending_drops(),
            static_cast<uint64_t>(kProducers * kPerProducer))
      << "every record arrived or was counted as dropped";
}

} // namespace
