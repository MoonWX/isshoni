// libFuzzer target for the SPS parser and the emulation-prevention transforms (docs/m2/04-windows-engine.md §9.4,
// §18.1): no crash, no out-of-bounds access (AddressSanitizer in host-fuzz), sane sizes, stable re-escaping and
// constraint rewrites. The properties are in sps_props.h; seeds in corpus/sps_fuzz/.
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <span>

#include "sps_props.h"

extern "C" int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size) {
  if (const char *err = im::h264::fuzzing::CheckSps(std::span<const uint8_t>(data, size))) {
    std::fprintf(stderr, "sps_fuzz: %s\n", err);
    std::abort();
  }
  return 0;
}
