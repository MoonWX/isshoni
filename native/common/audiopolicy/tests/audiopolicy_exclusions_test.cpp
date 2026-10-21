// Validation and copy of im_exclusions (docs/m2/05-windows-audio.md §4.3): normalization, default app ids, the implicit
// rule on the DLL's own process, and every rejection with the rule's index in im_last_error.
#include <gtest/gtest.h>

#include <string>
#include <vector>

#include "im/abi/status.h"
#include "im/audiopolicy/exclusions.h"
#include "isshoni_media.h"

namespace {

using im::abi::LastErrorText;
using im::audiopolicy::Exclusions;
using im::audiopolicy::IsValidAppId;
using im::audiopolicy::ParseExclusions;

im_exclusions Make(const std::vector<im_rule> &rules, uint32_t flags = 0, uint32_t shared = 0) {
  im_exclusions ex{};
  ex.struct_size = sizeof(im_exclusions);
  ex.flags = flags;
  ex.rules = rules.empty() ? nullptr : rules.data();
  ex.rule_count = static_cast<uint32_t>(rules.size());
  ex.shared_pid = shared;
  return ex;
}

TEST(AudioPolicyExclusions, CopiesAndNormalizes) {
  const std::vector<im_rule> rules = {
      {IM_RULE_APP_EXCLUDE, IM_RULE_SRC_BUILTIN, "Discord.EXE", "discord", "Discord"},
      {IM_RULE_APP_ALLOW, IM_RULE_SRC_USER, "Spotify.exe", nullptr, nullptr},
      {IM_RULE_STOP, IM_RULE_SRC_DATA, "Battle.net Launcher.exe", "", nullptr},
      {IM_RULE_WEBVIEW_DATA_DIR, IM_RULE_SRC_SELF, "C:/Users/test/AppData/Local/isshoni/WebView2/", nullptr, nullptr},
      {IM_RULE_INSTANCE, IM_RULE_SRC_SELF, "4120", nullptr, nullptr},
  };
  im_exclusions ex = Make(rules, IM_EXCLF_KEEP_OUT_MIC_USERS | IM_EXCLF_FAST_PATH | 0x100u, 77);
  Exclusions out;
  ASSERT_EQ(ParseExclusions(&ex, 999, out), IM_OK);
  EXPECT_EQ(out.flags, IM_EXCLF_KEEP_OUT_MIC_USERS | IM_EXCLF_FAST_PATH); // unknown bits are ignored
  EXPECT_EQ(out.shared_pid, 77u);
  ASSERT_EQ(out.rules.size(), 6u); // + the implicit self rule
  EXPECT_EQ(out.rules[0].value, "discord.exe");
  EXPECT_EQ(out.rules[0].app_id, "discord");
  EXPECT_EQ(out.rules[0].name, "Discord");
  EXPECT_EQ(out.rules[1].value, "spotify.exe");
  EXPECT_EQ(out.rules[1].app_id, "exe:spotify.exe");
  EXPECT_EQ(out.rules[1].name, "");
  EXPECT_EQ(out.rules[2].app_id, "exe:battle.net launcher.exe");
  EXPECT_EQ(out.rules[3].value, R"(c:\users\test\appdata\local\isshoni\webview2)");
  EXPECT_EQ(out.rules[3].app_id, "isshoni");
  EXPECT_EQ(out.rules[4].pid, 4120u);
  EXPECT_EQ(out.rules[4].app_id, "isshoni");
  EXPECT_EQ(out.rules[5].kind, IM_RULE_INSTANCE);
  EXPECT_EQ(out.rules[5].source, IM_RULE_SRC_SELF);
  EXPECT_EQ(out.rules[5].pid, 999u);
  EXPECT_EQ(out.rules[5].app_id, "isshoni");
}

TEST(AudioPolicyExclusions, ImplicitRuleEvenWithoutRules) {
  im_exclusions ex = Make({});
  Exclusions out;
  ASSERT_EQ(ParseExclusions(&ex, 42, out), IM_OK);
  ASSERT_EQ(out.rules.size(), 1u);
  EXPECT_EQ(out.rules[0].pid, 42u);
  ASSERT_EQ(ParseExclusions(&ex, 0, out), IM_OK);
  EXPECT_TRUE(out.rules.empty());
}

TEST(AudioPolicyExclusions, StructSizeRule) {
  Exclusions out;
  EXPECT_EQ(ParseExclusions(nullptr, 1, out), IM_ERR_INVALID_ARG);
  im_exclusions ex = Make({});
  ex.struct_size = 23;
  EXPECT_EQ(ParseExclusions(&ex, 1, out), IM_ERR_ABI);
  EXPECT_EQ(LastErrorText().substr(0, 4), "abi:");
  ex.struct_size = 0;
  EXPECT_EQ(ParseExclusions(&ex, 1, out), IM_ERR_ABI);
  // A newer caller's larger struct is accepted (only the v1.0 fields are read).
  struct Bigger {
    im_exclusions ex;
    uint64_t extra;
  } big{};
  big.ex = Make({});
  big.ex.struct_size = sizeof(Bigger);
  big.extra = ~0ull;
  EXPECT_EQ(ParseExclusions(&big.ex, 1, out), IM_OK);
}

TEST(AudioPolicyExclusions, RejectsBadRulesAndAppliesNothing) {
  const std::string long_value(IM_MAX_RULE_VALUE, 'a');    // 520 bytes: one too many
  const std::string max_value(IM_MAX_RULE_VALUE - 1, 'a'); // 519: fine
  const std::string long_id(IM_APP_ID_LEN, 'a');           // 64 bytes: one too many
  struct Case {
    const char *name;
    im_rule bad;
    im_status want;
  };
  const Case cases[] = {
      {"unknown kind", {7, IM_RULE_SRC_DATA, "x.exe", nullptr, nullptr}, IM_ERR_INVALID_ARG},
      {"unknown source", {IM_RULE_STOP, 5, "x.exe", nullptr, nullptr}, IM_ERR_INVALID_ARG},
      {"NULL value", {IM_RULE_STOP, IM_RULE_SRC_DATA, nullptr, nullptr, nullptr}, IM_ERR_INVALID_ARG},
      {"empty value", {IM_RULE_STOP, IM_RULE_SRC_DATA, "", nullptr, nullptr}, IM_ERR_INVALID_ARG},
      {"long value", {IM_RULE_STOP, IM_RULE_SRC_DATA, long_value.c_str(), nullptr, nullptr}, IM_ERR_INVALID_ARG},
      {"invalid UTF-8", {IM_RULE_STOP, IM_RULE_SRC_DATA, "bad\xC3", nullptr, nullptr}, IM_ERR_INVALID_ARG},
      {"pid with a letter", {IM_RULE_INSTANCE, IM_RULE_SRC_SELF, "12a", nullptr, nullptr}, IM_ERR_INVALID_ARG},
      {"pid 0", {IM_RULE_INSTANCE, IM_RULE_SRC_SELF, "0", nullptr, nullptr}, IM_ERR_INVALID_ARG},
      {"pid above 32 bits", {IM_RULE_INSTANCE, IM_RULE_SRC_SELF, "4294967296", nullptr, nullptr}, IM_ERR_INVALID_ARG},
      {"negative pid", {IM_RULE_INSTANCE, IM_RULE_SRC_SELF, "-1", nullptr, nullptr}, IM_ERR_INVALID_ARG},
      {"pid with a space", {IM_RULE_INSTANCE, IM_RULE_SRC_SELF, " 12", nullptr, nullptr}, IM_ERR_INVALID_ARG},
      {"upper-case app id", {IM_RULE_STOP, IM_RULE_SRC_DATA, "x.exe", "Discord", nullptr}, IM_ERR_INVALID_ARG},
      {"app id with a space", {IM_RULE_STOP, IM_RULE_SRC_DATA, "x.exe", "a b", nullptr}, IM_ERR_INVALID_ARG},
      {"long app id", {IM_RULE_STOP, IM_RULE_SRC_DATA, "x.exe", long_id.c_str(), nullptr}, IM_ERR_INVALID_ARG},
      {"name not UTF-8", {IM_RULE_STOP, IM_RULE_SRC_DATA, "x.exe", nullptr, "\xFF"}, IM_ERR_INVALID_ARG},
      {"separators only", {IM_RULE_WEBVIEW_DATA_DIR, IM_RULE_SRC_SELF, "//", nullptr, nullptr}, IM_ERR_INVALID_ARG},
  };
  for (const Case &c : cases) {
    SCOPED_TRACE(c.name);
    const std::vector<im_rule> rules = {
        {IM_RULE_STOP, IM_RULE_SRC_DATA, "explorer.exe", nullptr, nullptr},
        {IM_RULE_STOP, IM_RULE_SRC_DATA, max_value.c_str(), nullptr, nullptr},
        c.bad,
    };
    im_exclusions ex = Make(rules);
    Exclusions out;
    out.shared_pid = 1234; // a sentinel: nothing is applied on failure
    EXPECT_EQ(ParseExclusions(&ex, 1, out), c.want);
    EXPECT_NE(LastErrorText().find("im_exclusions.rules[2]"), std::string::npos) << LastErrorText();
    EXPECT_EQ(out.shared_pid, 1234u);
    EXPECT_TRUE(out.rules.empty());
  }
}

TEST(AudioPolicyExclusions, RejectsBadCounts) {
  Exclusions out;
  std::vector<im_rule> many(IM_MAX_RULES + 1, im_rule{IM_RULE_STOP, IM_RULE_SRC_DATA, "x.exe", nullptr, nullptr});
  im_exclusions ex = Make(many);
  EXPECT_EQ(ParseExclusions(&ex, 1, out), IM_ERR_INVALID_ARG);
  EXPECT_NE(LastErrorText().find("rule_count"), std::string::npos);
  many.resize(IM_MAX_RULES);
  ex = Make(many);
  EXPECT_EQ(ParseExclusions(&ex, 1, out), IM_OK);
  EXPECT_EQ(out.rules.size(), IM_MAX_RULES + 1u);
  ex = Make({});
  ex.rule_count = 2;
  EXPECT_EQ(ParseExclusions(&ex, 1, out), IM_ERR_INVALID_ARG);
}

TEST(AudioPolicyExclusions, CutsLongNamesOnACodePoint) {
  std::string name(IM_MAX_RULE_VALUE - 2, 'n');
  name += "\xC3\xA9\xC3\xA9"; // é é: the first ends past byte 519 - 1
  const std::vector<im_rule> rules = {{IM_RULE_APP_EXCLUDE, IM_RULE_SRC_USER, "x.exe", nullptr, name.c_str()}};
  im_exclusions ex = Make(rules);
  Exclusions out;
  ASSERT_EQ(ParseExclusions(&ex, 0, out), IM_OK);
  EXPECT_EQ(out.rules[0].name, std::string(IM_MAX_RULE_VALUE - 2, 'n'));
}

TEST(AudioPolicyExclusions, AppIdAlphabet) {
  EXPECT_TRUE(IsValidAppId("discord"));
  EXPECT_TRUE(IsValidAppId("exe:spotify.exe"));
  EXPECT_TRUE(IsValidAppId("a_b-c.d:9"));
  EXPECT_FALSE(IsValidAppId(""));
  EXPECT_FALSE(IsValidAppId(nullptr));
  EXPECT_FALSE(IsValidAppId("exe:My App.exe"));
  EXPECT_FALSE(IsValidAppId("\xC3\xA9"));
  EXPECT_TRUE(IsValidAppId(std::string(IM_APP_ID_LEN - 1, 'a').c_str()));
  EXPECT_FALSE(IsValidAppId(std::string(IM_APP_ID_LEN, 'a').c_str()));
}

} // namespace
