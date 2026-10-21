// libFuzzer target for the access-unit normalizer (docs/m2/04-windows-engine.md §9.4, §18.1): no crash, no
// out-of-bounds access (AddressSanitizer in host-fuzz), canonical and idempotent output. The properties are in
// annexb_props.h; seeds in corpus/annexb_fuzz/.
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <span>

#include "annexb_props.h"

extern "C" int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size) {
  if (const char *err = im::h264::fuzzing::CheckAnnexB(std::span<const uint8_t>(data, size))) {
    std::fprintf(stderr, "annexb_fuzz: %s\n", err);
    std::abort();
  }
  return 0;
}
