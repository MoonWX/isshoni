#include "im/audiomix/channel.h"

#include <new>
#include <utility>

namespace im::audiomix {

Channel::Channel(ChannelOptions &&options) noexcept
    : ring_(options.ring), tag_(options.tag), group_(options.group), crossfade_(options.crossfade),
      resampler_(std::move(options.resampler)), ctl_(options.controller) {}

Channel::~Channel() = default;

im_status Channel::Create(ChannelOptions options, std::unique_ptr<Channel> *out) {
  if (out == nullptr || options.ring == nullptr) {
    return IM_ERR_INVALID_ARG;
  }
  if (!options.resampler) {
    if (const im_status s = NewSpeexResampler(&options.resampler); s != IM_OK) {
      return s;
    }
  }
  auto *ch = new (std::nothrow) Channel(std::move(options));
  if (ch == nullptr) {
    return IM_ERR_INTERNAL;
  }
  out->reset(ch);
  return IM_OK;
}

} // namespace im::audiomix
