// The noexcept guards of the C ABI (docs/m2/04-windows-engine.md §13.4, README §4 "Errors"): no exception and no C++
// type crosses the ABI. Every export and every engine thread entry runs its body through Guard, which turns a C++
// exception (std::exception, and with catch (...) every other one, WinRT's hresult_error included) into
// IM_ERR_INTERNAL with a log record and the thread's detail. Access violations are SEH exceptions, which /EHsc never
// catches: they crash the process with Go's report, because the state would be corrupt.
//
// The exports themselves keep the header's declarations (a C header has no noexcept); Guard is what makes them never
// throw.
#pragma once

#include <exception>
#include <type_traits>
#include <utility>

#include "isshoni_media.h"

namespace im::api {

// Records an exception caught at `where` (an export's name, or a thread's): an error record in the log ring and the
// detail "<where>: unexpected exception: <what>". Returns IM_ERR_INTERNAL. what may be NULL.
im_status OnException(const char *where, const char *what) noexcept;

namespace detail {
template <class R> R InternalError(im_status st) noexcept {
  if constexpr (std::is_void_v<R>) {
    (void)st;
  } else {
    return static_cast<R>(st);
  }
}
} // namespace detail

// Runs f and returns its result; an exception becomes OnException's IM_ERR_INTERNAL (or nothing, for a void f).
template <class F> auto Guard(const char *where, F &&f) noexcept -> decltype(std::forward<F>(f)()) {
  using R = decltype(std::forward<F>(f)());
  try {
    return std::forward<F>(f)();
  } catch (const std::exception &e) {
    return detail::InternalError<R>(OnException(where, e.what()));
  } catch (...) {
    return detail::InternalError<R>(OnException(where, nullptr));
  }
}

} // namespace im::api
