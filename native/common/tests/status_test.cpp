// Status names, the detail tokens of docs/m2/04-windows-engine.md §5.7, the thread-local im_last_error detail and the
// struct_size rule of §5.1.
#include <gtest/gtest.h>

#include <set>
#include <string>
#include <thread>

#include "im/abi/status.h"
#include "im/abi/struct_size.h"

namespace {

using im::abi::Fail;
using im::abi::Failf;
using im::abi::LastErrorText;

TEST(Status, CodesKeepTheirValues) {
  // S1's values and 04 §5.2's; status codes are never renumbered or reused.
  static_assert(IM_OK == 0 && IM_TIMEOUT == 1 && IM_ENDED == 2);
  static_assert(IM_ERR_INVALID_ARG == -1 && IM_ERR_BAD_STATE == -2 && IM_ERR_ABI == -3);
  static_assert(IM_ERR_UNSUPPORTED_OS == -4 && IM_ERR_PERMISSION_DENIED == -5 && IM_ERR_AUDIO_DEVICE_CHANGED == -6);
  static_assert(IM_ERR_SOURCE_GONE == -7 && IM_ERR_ENCODER_LOST == -8 && IM_ERR_NO_ENCODER == -9);
  static_assert(IM_ERR_CANCELLED == -10 && IM_ERR_LIMIT == -11 && IM_ERR_DEPENDENCY == -12);
  static_assert(IM_ERR_AUDIO_UNAVAILABLE == -13 && IM_ERR_NOT_IMPLEMENTED == -14 && IM_ERR_TIMEOUT == -15);
  static_assert(IM_ERR_INTERNAL == -99);
  EXPECT_EQ(im::abi::StatusName(IM_ERR_TIMEOUT), "IM_ERR_TIMEOUT");
  EXPECT_EQ(im::abi::StatusName(IM_ENDED), "IM_ENDED");
  EXPECT_EQ(im::abi::StatusName(IM_ERR_INTERNAL), "IM_ERR_INTERNAL");
  EXPECT_EQ(im::abi::StatusName(-42), "IM_UNKNOWN");
  for (im_status s = -15; s <= 2; ++s) {
    EXPECT_NE(im::abi::StatusName(s), "IM_UNKNOWN") << s;
  }
}

TEST(Status, TokensOfSection57) {
  const auto tokens = im::abi::Tokens();
  EXPECT_EQ(tokens.size(), 23u);
  std::set<std::string_view> seen;
  for (const auto &t : tokens) {
    EXPECT_TRUE(seen.insert(t.token).second) << t.token;
    EXPECT_TRUE(im::abi::IsToken(t.token));
    // Tokens are snake_case ASCII, so they can start a detail and fill im_encoder_info.reason (char[32]).
    EXPECT_LT(t.token.size(), 32u);
    for (char c : t.token) {
      EXPECT_TRUE((c >= 'a' && c <= 'z') || c == '_') << t.token;
    }
  }
  EXPECT_FALSE(im::abi::IsToken("unknown_token"));
  EXPECT_FALSE(im::abi::IsToken(""));
  EXPECT_EQ(tokens[0].token, "missing");
  EXPECT_EQ(tokens[0].status, IM_ERR_DEPENDENCY);
  EXPECT_EQ(tokens.back().token, "abi");
  EXPECT_EQ(tokens.back().status, IM_ERR_ABI);
}

TEST(LastError, TokenPrefixAndClear) {
  im::abi::ClearLastError();
  EXPECT_EQ(LastErrorText(), "");
  EXPECT_EQ(Fail(IM_ERR_DEPENDENCY, im::abi::token::kBlockedByPolicy, "avcodec-62.dll: LoadLibraryExW failed (0x4EC)"),
            IM_ERR_DEPENDENCY);
  EXPECT_EQ(LastErrorText(), "blocked_by_policy: avcodec-62.dll: LoadLibraryExW failed (0x4EC)");
  EXPECT_EQ(Fail(IM_ERR_INVALID_ARG, {}, "im_start: cfg is NULL"), IM_ERR_INVALID_ARG);
  EXPECT_EQ(LastErrorText(), "im_start: cfg is NULL") << "no token, no prefix";
  EXPECT_EQ(Fail(IM_ERR_BAD_STATE, im::abi::token::kNotInitialized, ""), IM_ERR_BAD_STATE);
  EXPECT_EQ(LastErrorText(), "not_initialized: IM_ERR_BAD_STATE") << "a token is always followed by \": \"";
  EXPECT_EQ(Failf(IM_ERR_TIMEOUT, im::abi::token::kEngineTimeout, "%s", ""), IM_ERR_TIMEOUT);
  EXPECT_EQ(LastErrorText(), "engine_timeout: IM_ERR_TIMEOUT");
  EXPECT_EQ(Fail(IM_ERR_INVALID_ARG, {}, ""), IM_ERR_INVALID_ARG);
  EXPECT_EQ(LastErrorText(), "IM_ERR_INVALID_ARG") << "never empty after a failure";
  EXPECT_EQ(Failf(IM_ERR_LIMIT, im::abi::token::kSessionLimit, "%d of %d sessions in use", 4, 4), IM_ERR_LIMIT);
  EXPECT_EQ(LastErrorText(), "session_limit: 4 of 4 sessions in use");
  im::abi::ClearLastError();
  EXPECT_EQ(LastErrorText(), "");
}

TEST(LastError, OneLineAndBounded) {
  Fail(IM_ERR_INTERNAL, {}, "line one\nline two\r\tend");
  EXPECT_EQ(LastErrorText(), "line one line two  end");

  const std::string long_detail(5000, 'x');
  Fail(IM_ERR_INTERNAL, im::abi::token::kAbi, long_detail);
  EXPECT_EQ(LastErrorText().size(), im::abi::kMaxErrorBytes);
  EXPECT_EQ(LastErrorText().substr(0, 6), "abi: x");

  // Cut on a UTF-8 boundary: 1020 bytes of "x" then a 4-byte emoji does not fit in 1023.
  Fail(IM_ERR_INTERNAL, {}, std::string(1020, 'x') + "\xF0\x9F\x8E\xAE");
  EXPECT_EQ(LastErrorText().size(), 1020u);
}

TEST(LastError, WrapsTheCurrentDetail) {
  // An outer call that adds context to an inner failure passes the current detail back in.
  Fail(IM_ERR_DEPENDENCY, im::abi::token::kMissing, "avutil-60.dll");
  Fail(IM_ERR_DEPENDENCY, im::abi::token::kMissing, LastErrorText().substr(9));
  EXPECT_EQ(LastErrorText(), "missing: avutil-60.dll");
  const std::string inner(LastErrorText());
  Failf(IM_ERR_DEPENDENCY, im::abi::token::kMissing, "probe: %s", LastErrorText().data() + 9);
  EXPECT_EQ(LastErrorText(), "missing: probe: avutil-60.dll");
  EXPECT_EQ(inner, "missing: avutil-60.dll");
}

TEST(LastError, IsPerThread) {
  Fail(IM_ERR_INVALID_ARG, {}, "main thread");
  std::string other;
  std::thread t([&] {
    other = std::string(LastErrorText());
    Fail(IM_ERR_TIMEOUT, im::abi::token::kEngineTimeout, "worker");
  });
  t.join();
  EXPECT_EQ(other, "") << "a new thread starts without a detail";
  EXPECT_EQ(LastErrorText(), "main thread") << "another thread's failure doesn't touch this one";
}

TEST(LastError, CopiesWithTheSnprintfRule) {
  Fail(IM_ERR_SOURCE_GONE, im::abi::token::kPickExpired, "pick:7");
  char buf[64];
  EXPECT_EQ(im::abi::LastError(buf, static_cast<int32_t>(sizeof buf)), 20);
  EXPECT_STREQ(buf, "pick_expired: pick:7");
  EXPECT_EQ(im::abi::LastError(buf, 13), 20);
  EXPECT_STREQ(buf, "pick_expired");
  EXPECT_EQ(im::abi::LastError(nullptr, 0), 20);
}

TEST(StructSize, ReadInAcceptsOlderAndNewerCallers) {
  // A caller built with exactly v1.0.
  im_exclusions in{};
  in.struct_size = sizeof in;
  in.flags = IM_EXCLF_FAST_PATH;
  in.rule_count = 3;
  im_exclusions out{};
  ASSERT_EQ(im::abi::ReadIn(&in, out, "im_set_exclusions: ex"), IM_OK);
  EXPECT_EQ(out.flags, IM_EXCLF_FAST_PATH);
  EXPECT_EQ(out.rule_count, 3u);

  // A newer caller with a bigger struct: the engine reads its own size.
  struct Newer {
    im_exclusions v1;
    uint32_t added[4];
  } newer{};
  newer.v1 = in;
  newer.v1.struct_size = sizeof newer;
  ASSERT_EQ(im::abi::ReadIn(&newer.v1, out, "ex"), IM_OK);
  EXPECT_EQ(out.rule_count, 3u);
}

TEST(StructSize, BelowV10IsAbiError) {
  im_exclusions in{};
  in.struct_size = 20;
  im_exclusions out{};
  im::abi::ClearLastError();
  EXPECT_EQ(im::abi::ReadIn(&in, out, "im_audio_watch: ex"), IM_ERR_ABI);
  EXPECT_EQ(LastErrorText(), "abi: im_audio_watch: ex: struct_size 20 is below the v1.0 size 24");
  EXPECT_EQ(im::abi::ReadIn<im_exclusions>(nullptr, out, "im_audio_watch: ex"), IM_ERR_INVALID_ARG);
  EXPECT_EQ(LastErrorText(), "im_audio_watch: ex is NULL");

  im_packet pkt{};
  pkt.struct_size = 71;
  EXPECT_EQ(im::abi::CheckOut(&pkt, "im_next_packet: out"), IM_ERR_ABI);
  EXPECT_EQ(im::abi::CheckOut<im_packet>(nullptr, "im_next_packet: out"), IM_ERR_INVALID_ARG);
}

TEST(StructSize, WriteOutStopsAtTheCallersSize) {
  im_clock_sample value{};
  value.struct_size = 999; // ignored: WriteOut sets the bytes written
  value.mono_ns = 1;
  value.unix_ns = 2;
  value.uncertainty_ns = 3;

  struct Newer {
    im_clock_sample v1;
    int64_t added;
  } newer{};
  newer.v1.struct_size = sizeof newer;
  newer.added = 77;
  ASSERT_EQ(im::abi::CheckOut(&newer.v1, "im_clock: out"), IM_OK);
  im::abi::WriteOut(&newer.v1, value);
  EXPECT_EQ(newer.v1.struct_size, sizeof(im_clock_sample)) << "the engine wrote its own size";
  EXPECT_EQ(newer.v1.uncertainty_ns, 3);
  EXPECT_EQ(newer.added, 77) << "bytes beyond the engine's struct are left alone";

  im_clock_sample exact{};
  exact.struct_size = sizeof exact;
  im::abi::WriteOut(&exact, value);
  EXPECT_EQ(exact.struct_size, 32u);
  EXPECT_EQ(exact.mono_ns, 1);
}

} // namespace
