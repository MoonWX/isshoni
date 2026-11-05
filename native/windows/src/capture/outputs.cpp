#include "capture/outputs.h"

#include "im/json/writer.h"

namespace im::capture {

std::string MonitorIdOf(const DisplayOutput &output) {
  std::string id(kMonitorIdPrefix);
  id += output.device_name;
  return id;
}

std::optional<std::size_t> FindMonitor(std::span<const DisplayOutput> outputs, std::string_view id) noexcept {
  if (id.substr(0, kMonitorIdPrefix.size()) != kMonitorIdPrefix) {
    return std::nullopt;
  }
  const std::string_view device = id.substr(kMonitorIdPrefix.size());
  if (device.empty()) {
    return std::nullopt;
  }
  for (std::size_t i = 0; i < outputs.size(); ++i) {
    if (outputs[i].device_name == device) {
      return i;
    }
  }
  return std::nullopt;
}

uint32_t RotationDegrees(uint32_t dxgi_rotation) noexcept {
  switch (dxgi_rotation) {
  case kDxgiRotation90:
    return 90;
  case kDxgiRotation180:
    return 180;
  case kDxgiRotation270:
    return 270;
  default:
    return 0;
  }
}

std::string_view MonitorName(const DisplayOutput &output) noexcept {
  return output.friendly_name.empty() ? std::string_view(output.device_name) : std::string_view(output.friendly_name);
}

std::string SourcesJson(std::span<const DisplayOutput> outputs) {
  json::Writer w;
  w.BeginObject().Key("monitors").BeginArray();
  for (const DisplayOutput &o : outputs) {
    const PixelSize sz = o.size();
    w.BeginObject();
    w.Key("id").String(MonitorIdOf(o));
    w.Key("name").String(MonitorName(o));
    w.Key("primary").Bool(o.primary);
    w.Key("x").Int(o.left);
    w.Key("y").Int(o.top);
    w.Key("width").Int(sz.width);
    w.Key("height").Int(sz.height);
    w.Key("dpi").Uint(o.dpi);
    w.Key("refreshHz").Uint(o.refresh_hz);
    w.Key("rotation").Uint(RotationDegrees(o.rotation));
    w.Key("hdr").Bool(o.hdr);
    w.Key("adapter").Uint(o.adapter_index);
    w.EndObject();
  }
  w.EndArray().Key("windows").BeginArray().EndArray().EndObject();
  return w.Take();
}

MonitorMatch MatchPickedMonitor(std::span<const DisplayOutput> outputs, PixelSize item_size,
                                std::string_view display_name) {
  MonitorMatch m;
  if (item_size.width <= 0 || item_size.height <= 0) {
    return m; // step 5: nothing has no size
  }
  // MapMonitor reads views: they point into outputs, which outlive the call.
  std::vector<OutputDesc> descs;
  descs.reserve(outputs.size());
  for (const DisplayOutput &o : outputs) {
    OutputDesc d;
    d.device_name = o.device_name;
    d.friendly_name = o.friendly_name;
    d.left = o.left;
    d.top = o.top;
    d.right = o.right;
    d.bottom = o.bottom;
    descs.push_back(d);
  }
  m.result =
      MapMonitor(static_cast<uint32_t>(item_size.width), static_cast<uint32_t>(item_size.height), display_name, descs);
  if (m.result.mapped()) {
    m.output = outputs[static_cast<std::size_t>(m.result.output)];
  }
  return m;
}

std::string_view MapOutcomeName(MapResult::Outcome outcome) noexcept {
  switch (outcome) {
  case MapResult::Outcome::kBySize:
    return "size";
  case MapResult::Outcome::kByName:
    return "name";
  case MapResult::Outcome::kAmbiguous:
    return "ambiguous";
  case MapResult::Outcome::kNoSizeMatch:
    break;
  }
  return "no_size_match";
}

PickPlan PlanPick(const PickFacts &facts) {
  PickPlan p;
  const bool mapped = facts.match != nullptr && facts.match->output.has_value();
  if (facts.kind != IM_SOURCE_MONITOR) {
    p.capture_path = IM_CAPTURE_WGC;
    p.flags = (facts.borderless ? 0u : IM_SRCF_OS_BORDER) | (facts.self ? IM_SRCF_SELF : 0u);
    return p;
  }
  if (mapped) {
    p.monitor_id = MonitorIdOf(*facts.match->output);
  }
  if (!facts.windows11 && mapped) {
    p.capture_path = IM_CAPTURE_DD; // §7.5: borderless on Windows 10
    p.flags = IM_SRCF_MAPPED;
    return p;
  }
  // Windows 11 (borderless where allowed), or Windows 10's step 5: WGC with the border.
  p.capture_path = IM_CAPTURE_WGC;
  p.flags = facts.borderless ? 0u : IM_SRCF_OS_BORDER;
  return p;
}

} // namespace im::capture
