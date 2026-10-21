// The struct_size rule of the C ABI (docs/m2/04-windows-engine.md §5.1): the engine reads an input struct up to
// min(struct_size, its own sizeof) and treats missing fields as zero; it writes an output struct up to
// min(struct_size, its own sizeof) and sets struct_size to the bytes it wrote. A struct_size below the v1.0 size is
// IM_ERR_ABI (token abi); a larger one (a newer caller) is accepted.
#pragma once

#include <algorithm>
#include <concepts>
#include <cstddef>
#include <cstring>
#include <type_traits>

#include "im/abi/layout.h"
#include "im/abi/status.h"

namespace im::abi {

template <class T>
concept TopLevelStruct = std::is_trivially_copyable_v<T> && std::is_standard_layout_v<T> && requires(T t) {
  { t.struct_size } -> std::convertible_to<uint32_t>;
  V1Size<T>::value;
};

// Copies the caller's input struct into out (zero-filled first). what names the argument for the detail
// ("im_init: params").
template <TopLevelStruct T> im_status ReadIn(const T *in, T &out, const char *what) noexcept {
  if (in == nullptr) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s is NULL", what);
  }
  const uint32_t n = in->struct_size;
  if (n < V1Size<T>::value) {
    return Failf(IM_ERR_ABI, token::kAbi, "%s: struct_size %u is below the v1.0 size %u", what,
                 static_cast<unsigned>(n), static_cast<unsigned>(V1Size<T>::value));
  }
  out = T{};
  std::memcpy(&out, in, std::min<std::size_t>(n, sizeof(T)));
  return IM_OK;
}

// Checks the caller's output struct before the export does any work.
template <TopLevelStruct T> im_status CheckOut(const T *out, const char *what) noexcept {
  if (out == nullptr) {
    return Failf(IM_ERR_INVALID_ARG, {}, "%s is NULL", what);
  }
  const uint32_t n = out->struct_size;
  if (n < V1Size<T>::value) {
    return Failf(IM_ERR_ABI, token::kAbi, "%s: struct_size %u is below the v1.0 size %u", what,
                 static_cast<unsigned>(n), static_cast<unsigned>(V1Size<T>::value));
  }
  return IM_OK;
}

// Writes value into the caller's struct (which passed CheckOut): min(out->struct_size, sizeof(T)) bytes, with
// struct_size set to that count. Bytes beyond it are left alone.
template <TopLevelStruct T> void WriteOut(T *out, const T &value) noexcept {
  const std::size_t n = std::min<std::size_t>(out->struct_size, sizeof(T));
  T tmp = value;
  tmp.struct_size = static_cast<uint32_t>(n);
  std::memcpy(out, &tmp, n);
}

} // namespace im::abi
