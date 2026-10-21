// Deep copies of the ABI's input structs (docs/m2/04-windows-engine.md §5.1: the engine copies everything it needs
// before a call returns and never keeps a pointer into caller memory). A posted job owns these, so it never reads the
// caller's memory, which may be gone once the export timed out (04 §6.1). Neither type can be copied or moved: their
// views point into themselves.
#pragma once

#include <optional>
#include <string>
#include <vector>

#include "isshoni_media.h"

namespace im::api {

// An im_exclusions with its rule array and every string. ex must have passed CheckExclusions.
class OwnedExclusions {
public:
  explicit OwnedExclusions(const im_exclusions &ex);
  OwnedExclusions(const OwnedExclusions &) = delete;
  OwnedExclusions &operator=(const OwnedExclusions &) = delete;

  // The copy as an im_exclusions (struct_size = sizeof(im_exclusions)); valid while this object lives.
  const im_exclusions *view() const noexcept { return &view_; }

private:
  struct Rule {
    uint32_t kind;
    uint32_t source;
    std::string value;
    std::optional<std::string> app_id;
    std::optional<std::string> name;
  };
  std::vector<Rule> rules_;
  std::vector<im_rule> rule_views_;
  im_exclusions view_{};
};

// im_start's input: the caller's im_start_config (up to its struct_size, the rest zero) whose source_id and
// exclusions point into this object. cfg must have passed CheckStartConfig.
class StartRequest {
public:
  explicit StartRequest(const im_start_config &cfg);
  StartRequest(const StartRequest &) = delete;
  StartRequest &operator=(const StartRequest &) = delete;

  const im_start_config &config() const noexcept { return cfg_; }
  // No source and no layers: an audio-only session (04 §10.3).
  bool audio_only() const noexcept { return cfg_.source_id == nullptr; }

private:
  im_start_config cfg_{};
  std::string source_id_;
  std::optional<OwnedExclusions> exclusions_;
};

} // namespace im::api
