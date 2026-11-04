// "What friends hear" (docs/m2/05-windows-audio.md §12.1; "05" below) as the watcher builds it. The model and its JSON
// writer are im_audiomix's (native/common/audiomix/include/im/audiomix/report.h, README WA36): the one C++ writer of
// the report, checked byte for byte against the goldens in internal/media/testdata/audio/ (mix_report_test). README
// WA45 removed this folder's interim writer (README §3 item 36). What stays here is the watcher's side: the planner's
// vocabulary in the report's, and what the report's seq covers.
//
// Names, exe names and app ids are local data that the report carries to Go and never into a log (05 §16).
#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

#include "im/audiomix/report.h"
#include "im/audiopolicy/planner.h"
#include "im/audiopolicy/types.h"

namespace im::audio {

using audiomix::Report;
using audiomix::ReportEvent;

// The report's limits (05 §12.1, §17), as the writer applies them.
inline constexpr std::size_t kReportMaxEvents = audiomix::Report::kMaxEvents;

// The planner's vocabulary in the report's.
audiomix::ReportEngine ToReport(audiopolicy::Engine e) noexcept;
audiomix::AppVerdict ToReport(audiopolicy::Verdict v) noexcept;
audiomix::AppReason ToReport(audiopolicy::Reason r) noexcept;
audiomix::SessionNote ToReport(audiopolicy::Note n) noexcept;
audiomix::SessionFlow ToReport(audiopolicy::Flow f) noexcept;
audiomix::SessionState ToReport(audiopolicy::SessionState s) noexcept;
// A row's kWarn* bits by name, lowest bit first.
std::vector<audiomix::AppWarning> WarningsOf(uint32_t bits);
// One row of the plan (05 §12.1 apps[]).
audiomix::ReportApp ToReport(const audiopolicy::AppRow &row);
// A plan's report-level warning ("multi_process_session", "instance_gone"); false for a code the report doesn't know.
bool ToReport(const audiopolicy::PlanWarning &w, audiomix::ReportWarning *out);

// What seq covers (05 §12.1: everything except levels, counters, qpcNs and events), as a string to compare: the
// watcher increments seq when it changes.
std::string VisibleState(const Report &r);

} // namespace im::audio
