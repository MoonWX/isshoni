// The skeleton's window protection (README WA11): not implemented yet. README WA70 replaces it.
#include "window/protect.h"

#include "im/abi/status.h"

namespace im::window {

im_status Protect(void *, uint32_t) {
  return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "im_protect_window: not implemented yet (README WA70)");
}

} // namespace im::window
