#include "api/guard.h"

#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::api {

im_status OnException(const char *where, const char *what) noexcept {
  const char *w = where != nullptr ? where : "engine";
  const char *text = what != nullptr ? what : "not a std::exception";
  log::Error("api", "unexpected exception", {{"in", w}, {"what", text}});
  return abi::Failf(IM_ERR_INTERNAL, {}, "%s: unexpected exception: %s", w, text);
}

} // namespace im::api
