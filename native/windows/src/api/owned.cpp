#include "api/owned.h"

#include <utility>

namespace im::api {

OwnedExclusions::OwnedExclusions(const im_exclusions &ex) {
  view_.struct_size = sizeof(im_exclusions);
  view_.flags = ex.flags;
  view_.shared_pid = ex.shared_pid;
  rules_.reserve(ex.rule_count);
  for (uint32_t i = 0; i < ex.rule_count; ++i) {
    const im_rule &r = ex.rules[i];
    Rule copy{r.kind, r.source, r.value != nullptr ? r.value : "", std::nullopt, std::nullopt};
    if (r.app_id != nullptr) {
      copy.app_id.emplace(r.app_id);
    }
    if (r.name != nullptr) {
      copy.name.emplace(r.name);
    }
    rules_.push_back(std::move(copy));
  }
  // The strings no longer move: point the views at them.
  rule_views_.reserve(rules_.size());
  for (const Rule &r : rules_) {
    rule_views_.push_back(im_rule{r.kind, r.source, r.value.c_str(), r.app_id ? r.app_id->c_str() : nullptr,
                                  r.name ? r.name->c_str() : nullptr});
  }
  view_.rules = rule_views_.empty() ? nullptr : rule_views_.data();
  view_.rule_count = static_cast<uint32_t>(rule_views_.size());
}

StartRequest::StartRequest(const im_start_config &cfg) : cfg_(cfg) {
  if (cfg.source_id != nullptr) {
    source_id_ = cfg.source_id;
    cfg_.source_id = source_id_.c_str();
  }
  if (cfg.exclusions != nullptr) {
    exclusions_.emplace(*cfg.exclusions);
    cfg_.exclusions = exclusions_->view();
  }
}

} // namespace im::api
