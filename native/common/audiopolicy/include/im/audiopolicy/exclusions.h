// The rules of im_exclusions, validated and copied (docs/m2/05-windows-audio.md §4.1, §4.3, §5.3, §14.3).
//
// Go builds the rules (internal/desktop/audio, 05 §14.3): isshoni itself, the data (launchers, shared runtimes,
// browsers), the built-in voice apps, the user's decisions and the viewer rules. The DLL copies them during the call
// (04 §5.1) with ParseExclusions, which also adds the implicit rule on the DLL's own process (05 §4.3), and hands the
// result to the Planner.
#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "isshoni_media.h"

namespace im::audiopolicy {

// One validated rule. value is normalized for matching: exe names case-folded (FoldCase), the WebView2 data folder
// normalized (NormalizePath), an instance rule's pid in pid.
struct Rule {
  uint32_t kind = IM_RULE_APP_EXCLUDE; // IM_RULE_*
  uint32_t source = IM_RULE_SRC_DATA;  // IM_RULE_SRC_*
  std::string value;
  uint32_t pid = 0;   // IM_RULE_INSTANCE only
  std::string app_id; // the report id of matches: the given one, else "exe:<value>" ("isshoni" for the two row-1
                      // kinds IM_RULE_INSTANCE and IM_RULE_WEBVIEW_DATA_DIR, whose value is not an exe)
  std::string name;   // the display name, "" = the app root's FileDescription (05 §5.1)
};

struct Exclusions {
  uint32_t flags = IM_EXCLF_KEEP_OUT_MIC_USERS | IM_EXCLF_FAST_PATH; // IM_EXCLF_*; unknown bits are dropped
  uint32_t shared_pid = 0;                                           // the shared window's process, 0 = none
  std::vector<Rule> rules;                                           // in the caller's order, the implicit one last
};

// The app id of the implicit rule on the DLL's own process (05 §4.3).
inline constexpr const char *kSelfAppId = "isshoni";

// Validates and copies *ex into out (04 §5.1, 05 §4.3) and appends IM_RULE_INSTANCE(self_pid) with source SELF and app
// id "isshoni" when self_pid is not 0 (the DLL passes GetCurrentProcessId(), whatever Go sent). Failures set the
// thread's im_last_error detail (im::abi::Fail) and leave out unchanged:
//   - ex NULL: IM_ERR_INVALID_ARG; struct_size below the v1.0 size (24): IM_ERR_ABI with token abi;
//   - rule_count > IM_MAX_RULES, or rules NULL with rule_count > 0: IM_ERR_INVALID_ARG;
//   - a rule with an unknown kind or source, a NULL, empty or invalid UTF-8 value, a value of IM_MAX_RULE_VALUE bytes
//     or more, a malformed pid (not 1..4294967295 in decimal digits), an app_id outside [a-z0-9:._-]{1,63}, or a name
//     that is not valid UTF-8: IM_ERR_INVALID_ARG naming the rule's index ("im_exclusions.rules[3]: ...").
// A name is cut to IM_MAX_RULE_VALUE - 1 bytes on a UTF-8 boundary. Throws only std::bad_alloc (the export catches it).
im_status ParseExclusions(const im_exclusions *ex, uint32_t self_pid, Exclusions &out);

// Whether s is a valid explicit app id: [a-z0-9:._-]{1,63} (05 §4.3).
bool IsValidAppId(const char *s) noexcept;

} // namespace im::audiopolicy
