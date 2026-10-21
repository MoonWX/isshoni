// The JSON writer, the UTF-8 helpers and the snprintf rule (docs/m2/04-windows-engine.md §5.1, §18.1): escaping of
// quotes and control characters, emoji, invalid UTF-8 and lone surrogates as U+FFFD, numbers, structure, and the return
// values of CopyOut.
#include <gtest/gtest.h>

#include <cmath>
#include <limits>
#include <string>

#include "im/abi/status.h"
#include "im/abi/utf8.h"
#include "im/json/writer.h"

namespace {

using im::json::Writer;

std::string Esc(std::string_view s) {
  std::string out;
  im::json::AppendEscaped(out, s);
  return out;
}

std::string Esc16(std::u16string_view s) {
  std::string out;
  im::json::AppendEscaped(out, s);
  return out;
}

const std::string kFFFD = "\xEF\xBF\xBD";

TEST(JsonEscape, QuotesBackslashesAndControlCharacters) {
  EXPECT_EQ(Esc("plain text 123"), "plain text 123");
  EXPECT_EQ(Esc("say \"hi\""), "say \\\"hi\\\"");
  EXPECT_EQ(Esc("C:\\Users"), "C:\\\\Users");
  EXPECT_EQ(Esc("a\nb\rc\td\be\ff"), "a\\nb\\rc\\td\\be\\ff");
  EXPECT_EQ(Esc(std::string_view("\x00\x01\x1f", 3)), "\\u0000\\u0001\\u001f");
  EXPECT_EQ(Esc("\x7f"), "\x7f") << "DEL is not a JSON control character";
  EXPECT_EQ(Esc("/"), "/");
  EXPECT_EQ(Esc("\xE2\x80\xA8\xE2\x80\xA9"), "\\u2028\\u2029") << "escaped like Go's encoding/json";
}

TEST(JsonEscape, Utf8PassesThrough) {
  EXPECT_EQ(Esc("caf\xC3\xA9"), "caf\xC3\xA9");
  EXPECT_EQ(Esc("\xE6\x97\xA5\xE6\x9C\xAC"), "\xE6\x97\xA5\xE6\x9C\xAC"); // CJK
  EXPECT_EQ(Esc("\xF0\x9F\x8E\xAE game"), "\xF0\x9F\x8E\xAE game");       // emoji (U+1F3AE)
  EXPECT_EQ(Esc(kFFFD), kFFFD) << "U+FFFD itself is valid";
  EXPECT_EQ(Esc("\xF4\x8F\xBF\xBF"), "\xF4\x8F\xBF\xBF") << "U+10FFFF";
}

TEST(JsonEscape, InvalidUtf8BecomesReplacementCharacters) {
  EXPECT_EQ(Esc("a\x80z"), "a" + kFFFD + "z") << "stray continuation byte";
  EXPECT_EQ(Esc("\xC0\xAF"), kFFFD + kFFFD) << "overlong";
  EXPECT_EQ(Esc("\xE0\x80\xAF"), kFFFD + kFFFD + kFFFD) << "overlong 3-byte form";
  EXPECT_EQ(Esc("\xF5\x80"), kFFFD + kFFFD) << "above U+10FFFF";
  EXPECT_EQ(Esc("\xF4\x90\x80\x80"), kFFFD + kFFFD + kFFFD + kFFFD) << "above U+10FFFF";
  EXPECT_EQ(Esc("\xE6\x97"), kFFFD) << "a cut sequence is one maximal subpart";
  EXPECT_EQ(Esc("\xE6\x97x"), kFFFD + "x");
  EXPECT_EQ(Esc("\xFF"), kFFFD);
}

TEST(JsonEscape, LoneSurrogatesBecomeReplacementCharacters) {
  // Surrogates encoded in UTF-8 (CESU-8 / WTF-8) are invalid: ED A0..BF is not a valid start.
  EXPECT_EQ(Esc("\xED\xA0\x80"), kFFFD + kFFFD + kFFFD) << "lone high surrogate U+D800";
  EXPECT_EQ(Esc("\xED\xBF\xBF"), kFFFD + kFFFD + kFFFD) << "lone low surrogate U+DFFF";
  EXPECT_EQ(Esc("\xED\x9F\xBF"), "\xED\x9F\xBF") << "U+D7FF is fine";

  // From UTF-16 (window titles on Windows).
  EXPECT_EQ(Esc16(u"title"), "title");
  EXPECT_EQ(Esc16(u"\u00e9\u65e5"), "\xC3\xA9\xE6\x97\xA5");
  EXPECT_EQ(Esc16(u"\U0001F3AE"), "\xF0\x9F\x8E\xAE") << "a surrogate pair is one emoji";
  const char16_t lone_high[] = {u'a', 0xD83C, u'b', 0};
  EXPECT_EQ(Esc16(lone_high), "a" + kFFFD + "b");
  const char16_t lone_low[] = {0xDFAE, 0};
  EXPECT_EQ(Esc16(lone_low), kFFFD);
  const char16_t reversed[] = {0xDFAE, 0xD83C, 0};
  EXPECT_EQ(Esc16(reversed), kFFFD + kFFFD);
  const char16_t high_at_end[] = {u'x', 0xD83C, 0};
  EXPECT_EQ(Esc16(high_at_end), "x" + kFFFD);
  EXPECT_EQ(Esc16(u"\"\n"), "\\\"\\n");
}

TEST(Utf8, Helpers) {
  EXPECT_TRUE(im::abi::IsValidUtf8("caf\xC3\xA9 \xF0\x9F\x8E\xAE"));
  EXPECT_TRUE(im::abi::IsValidUtf8(kFFFD));
  EXPECT_FALSE(im::abi::IsValidUtf8("\xED\xA0\x80"));
  EXPECT_FALSE(im::abi::IsValidUtf8("\xC3"));
  EXPECT_EQ(im::abi::SanitizeUtf8("a\xC3z"), "a" + kFFFD + "z");

  const std::string s = "ab\xC3\xA9\xF0\x9F\x8E\xAE"; // a b é 🎮: 2 + 2 + 4 bytes
  EXPECT_EQ(im::abi::Utf8PrefixLen(s, 100), s.size());
  EXPECT_EQ(im::abi::Utf8PrefixLen(s, 2), 2u);
  EXPECT_EQ(im::abi::Utf8PrefixLen(s, 3), 2u) << "never inside é";
  EXPECT_EQ(im::abi::Utf8PrefixLen(s, 4), 4u);
  EXPECT_EQ(im::abi::Utf8PrefixLen(s, 5), 4u) << "never inside the emoji";
  EXPECT_EQ(im::abi::Utf8PrefixLen(s, 7), 4u);
  EXPECT_EQ(im::abi::Utf8PrefixLen(s, 8), 8u);
  EXPECT_EQ(im::abi::Utf8PrefixLen("a\x80\x80\x80\x80z", 3), 3u) << "stray continuation bytes are cut where asked";

  char field[4];
  EXPECT_EQ(im::abi::CopyFixed(field, "ab\xC3\xA9xyz"), 2u) << "3 bytes would end inside \xC3\xA9";
  EXPECT_STREQ(field, "ab");
  char wider[6];
  EXPECT_EQ(im::abi::CopyFixed(wider, "ab\xC3\xA9xyz"), 5u);
  EXPECT_STREQ(wider, "ab\xC3\xA9x");
  char tiny[4];
  EXPECT_EQ(im::abi::CopyFixed(tiny, "\xF0\x9F\x8E\xAE"), 0u);
  EXPECT_STREQ(tiny, "");
  char rid[4] = {'x', 'x', 'x', 'x'};
  im::abi::CopyFixed(rid, "f");
  EXPECT_EQ(rid[1], '\0');
  EXPECT_EQ(rid[3], '\0') << "the rest of the field is zeroed";
}

TEST(JsonWriter, Structure) {
  Writer w;
  w.BeginObject();
  w.Key("monitors").BeginArray();
  w.BeginObject().Key("id").String("mon:\\\\.\\DISPLAY1").Key("primary").Bool(true).Key("x").Int(-1920).EndObject();
  w.BeginObject().Key("id").String("mon:2").Key("primary").Bool(false).Key("dpi").Uint(144).EndObject();
  w.EndArray();
  w.Key("windows").BeginArray().EndArray();
  w.Key("empty").BeginObject().EndObject();
  w.Key("none").Null();
  w.Key("raw").Raw("[1,2]");
  w.EndObject();
  EXPECT_EQ(w.str(), "{\"monitors\":[{\"id\":\"mon:\\\\\\\\.\\\\DISPLAY1\",\"primary\":true,\"x\":-1920},"
                     "{\"id\":\"mon:2\",\"primary\":false,\"dpi\":144}],\"windows\":[],\"empty\":{},\"none\":null,"
                     "\"raw\":[1,2]}");
}

TEST(JsonWriter, ArraysOfScalarsAndNesting) {
  Writer w;
  w.BeginArray().Int(1).Int(2).BeginArray().String("a").EndArray().BeginObject().EndObject().Bool(false).EndArray();
  EXPECT_EQ(w.str(), "[1,2,[\"a\"],{},false]");
  const std::string taken = w.Take();
  EXPECT_EQ(taken, "[1,2,[\"a\"],{},false]");
  EXPECT_EQ(w.str(), "");
  w.BeginObject().Key("k\"ey").String(u"v\u00e9").EndObject();
  EXPECT_EQ(w.str(), "{\"k\\\"ey\":\"v\xC3\xA9\"}") << "keys are escaped too; a reused writer starts fresh";
}

TEST(JsonWriter, Numbers) {
  Writer w;
  w.BeginArray();
  w.Int(0).Int(std::numeric_limits<int64_t>::min()).Uint(std::numeric_limits<uint64_t>::max());
  w.Double(60.0).Double(59.8).Double(0.1).Double(-0.0).Double(1e-7).Double(1e21);
  w.Double(std::nan("")).Double(std::numeric_limits<double>::infinity());
  w.Fixed(59.84, 1).Fixed(3.1, 2).Fixed(-0.04, 1).Fixed(7.5, 0).Fixed(std::nan(""), 1);
  w.EndArray();
  EXPECT_EQ(w.str(), "[0,-9223372036854775808,18446744073709551615,60,59.8,0.1,0,1e-07,1e+21,null,null,59.8,3.10,0.0,"
                     "8,null]");
}

TEST(JsonWriter, CopyOutFollowsSnprintf) {
  Writer w;
  w.BeginObject().Key("a").Int(1).EndObject(); // {"a":1} = 7 bytes
  char buf[16];
  EXPECT_EQ(w.CopyOut(buf, static_cast<int32_t>(sizeof buf)), 7);
  EXPECT_STREQ(buf, "{\"a\":1}");
  EXPECT_EQ(w.CopyOut(buf, 8), 7) << "exactly n + 1 fits";
  EXPECT_STREQ(buf, "{\"a\":1}");
  EXPECT_EQ(w.CopyOut(buf, 7), 7) << "n >= cap: truncated, the caller retries with n + 1";
  EXPECT_STREQ(buf, "{\"a\":1");
  EXPECT_EQ(w.CopyOut(buf, 1), 7);
  EXPECT_STREQ(buf, "");
  buf[0] = 'x';
  EXPECT_EQ(w.CopyOut(buf, 0), 7) << "cap 0 asks for the length and writes nothing";
  EXPECT_EQ(buf[0], 'x');
  EXPECT_EQ(w.CopyOut(nullptr, 0), 7);
  EXPECT_EQ(w.CopyOut(nullptr, 4), IM_ERR_INVALID_ARG);
  EXPECT_EQ(w.CopyOut(buf, -1), IM_ERR_INVALID_ARG);
}

TEST(JsonWriter, CopyOutOfAnEmptyDocument) {
  char buf[4] = {'x', 'x', 'x', 'x'};
  EXPECT_EQ(im::abi::CopyOut("", buf, static_cast<int32_t>(sizeof buf)), 0);
  EXPECT_EQ(buf[0], '\0');
}

} // namespace
