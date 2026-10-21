// The struct layouts of the C ABI (docs/m2/04-windows-engine.md §5.4): sizeof and offsetof of every field of every
// struct in isshoni_media.h, for im_abi_layout and the layout tests. The golden file
// native/common/tests/testdata/abi_layout.json holds the same document; Go's layout_test.go and the Windows DLL test
// read it too.
#pragma once

#include <cstddef>
#include <cstdint>
#include <span>
#include <string>
#include <string_view>

#include "isshoni_media.h"

namespace im::abi {

struct FieldLayout {
  std::string_view name;
  std::size_t offset;
};

struct StructLayout {
  std::string_view name;
  std::size_t size;
  std::span<const FieldLayout> fields; // in declaration order
};

// Every struct of the header, in header order.
std::span<const StructLayout> Layouts() noexcept;

// The im_abi_layout document: {"abi":65536,"structs":{"im_init_params":{"size":32,"fields":{"struct_size":0,...}},...}}
// in header order, compact (no whitespace).
std::string LayoutJson();

// V1Size<T>::value is the v1.0 size of a top-level struct (one that starts with struct_size): a struct_size below it
// is IM_ERR_ABI (04 §5.1). It stays at the v1.0 number when a later minor appends fields. Structs embedded by value or
// used as array elements have no struct_size and no V1Size.
template <class T> struct V1Size;
template <> struct V1Size<im_init_params> {
  static constexpr uint32_t value = 32;
};
template <> struct V1Size<im_clock_sample> {
  static constexpr uint32_t value = 32;
};
template <> struct V1Size<im_caps> {
  static constexpr uint32_t value = 1568;
};
template <> struct V1Size<im_pick_params> {
  static constexpr uint32_t value = 24;
};
template <> struct V1Size<im_source_info> {
  static constexpr uint32_t value = 440;
};
template <> struct V1Size<im_exclusions> {
  static constexpr uint32_t value = 24;
};
template <> struct V1Size<im_audio_levels> {
  static constexpr uint32_t value = 5152;
};
template <> struct V1Size<im_start_config> {
  static constexpr uint32_t value = 224;
};
template <> struct V1Size<im_session_info> {
  static constexpr uint32_t value = 184;
};
template <> struct V1Size<im_packet> {
  static constexpr uint32_t value = 72;
};

} // namespace im::abi
