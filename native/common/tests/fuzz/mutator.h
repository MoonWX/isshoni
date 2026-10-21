// A small deterministic input mutator for the fuzz properties' runs inside the unit tests (annexb_test, sps_test):
// bit flips, byte changes, inserts, deletes, chunk copies, start-code inserts and splices of the seeds. Not a
// replacement for libFuzzer's coverage-guided runs (host-fuzz, the nightly); it makes every build check the
// properties on a few thousand malformed inputs.
#pragma once

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <iterator>
#include <random>
#include <vector>

namespace im::fuzzing {

class Mutator {
public:
  explicit Mutator(uint32_t seed) : rng_(seed) {}

  std::vector<uint8_t> Mutate(const std::vector<uint8_t> &input, const std::vector<std::vector<uint8_t>> &seeds) {
    std::vector<uint8_t> v = input;
    const std::size_t steps = 1 + Below(4);
    for (std::size_t s = 0; s < steps; ++s) {
      switch (Below(8)) {
      case 0: // flip a bit
        if (!v.empty()) {
          const std::size_t at = Below(v.size());
          v[at] = static_cast<uint8_t>(v[at] ^ (1u << Below(8)));
        }
        break;
      case 1: // set a byte to an interesting value
        if (!v.empty()) {
          static constexpr uint8_t kValues[] = {0x00, 0x01, 0x03, 0x67, 0x68, 0x65, 0x41, 0x09, 0x06, 0x0C, 0xFF, 0x80};
          v[Below(v.size())] = kValues[Below(sizeof(kValues))];
        }
        break;
      case 2: // insert a random byte
        v.insert(v.begin() + static_cast<std::ptrdiff_t>(Below(v.size() + 1)), static_cast<uint8_t>(Below(256)));
        break;
      case 3: // delete a run
        if (!v.empty()) {
          const std::size_t at = Below(v.size());
          const std::size_t n = 1 + Below(std::min<std::size_t>(8, v.size() - at));
          v.erase(v.begin() + static_cast<std::ptrdiff_t>(at), v.begin() + static_cast<std::ptrdiff_t>(at + n));
        }
        break;
      case 4: // copy a chunk elsewhere
        if (v.size() > 2) {
          const std::size_t from = Below(v.size());
          const std::size_t n = 1 + Below(std::min<std::size_t>(32, v.size() - from));
          const std::vector<uint8_t> chunk(v.begin() + static_cast<std::ptrdiff_t>(from),
                                           v.begin() + static_cast<std::ptrdiff_t>(from + n));
          v.insert(v.begin() + static_cast<std::ptrdiff_t>(Below(v.size() + 1)), chunk.begin(), chunk.end());
        }
        break;
      case 5: { // insert a start code
        static constexpr uint8_t kCode[] = {0, 0, 0, 1};
        const std::size_t skip = Below(2);
        v.insert(v.begin() + static_cast<std::ptrdiff_t>(Below(v.size() + 1)), std::begin(kCode) + skip,
                 std::end(kCode));
        break;
      }
      case 6: // splice with a seed
        if (!seeds.empty()) {
          const auto &other = seeds[Below(seeds.size())];
          const std::size_t keep = Below(v.size() + 1);
          const std::size_t from = Below(other.size() + 1);
          v.resize(keep);
          v.insert(v.end(), other.begin() + static_cast<std::ptrdiff_t>(from), other.end());
        }
        break;
      default: // truncate
        if (!v.empty()) {
          v.resize(Below(v.size()));
        }
        break;
      }
    }
    if (v.size() > kMaxSize) {
      v.resize(kMaxSize);
    }
    return v;
  }

private:
  static constexpr std::size_t kMaxSize = 8192;

  std::size_t Below(std::size_t n) {
    if (n == 0) {
      return 0;
    }
    return static_cast<std::size_t>(rng_() % n);
  }

  std::mt19937 rng_;
};

} // namespace im::fuzzing
