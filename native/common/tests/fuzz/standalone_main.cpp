// A stand-in for libFuzzer's driver on hosts whose clang ships no libFuzzer runtime (Apple's clang, so the owner's
// Mac): runs every seed of a corpus folder, then mutates for the given seconds (the seeds and a rolling pool of
// mutants, with the unit tests' Mutator) and calls the target's LLVMFuzzerTestOneInput on each input. It has no
// coverage feedback, so it complements the host-fuzz preset's libFuzzer runs (the nightly) and doesn't replace them.
// CMake doesn't build it (only tests/fuzz/*_fuzz.cpp are fuzz targets). From native/, for example:
//
//   clang++ -std=c++20 -O1 -g -fsanitize=address,undefined -fno-sanitize-recover=all -Iinclude -Icommon/include \
//     -Icommon/tests common/tests/fuzz/standalone_main.cpp common/tests/fuzz/annexb_fuzz.cpp common/src/h264/*.cpp \
//     -o build/annexb_fuzz_standalone
//   build/annexb_fuzz_standalone common/tests/fuzz/corpus/annexb_fuzz 60
//
// (sps_fuzz the same with sps_fuzz.cpp and corpus/sps_fuzz.) A property failure aborts inside the target.
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iterator>
#include <utility>
#include <vector>

#include "fuzz/mutator.h"

extern "C" int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size);

int main(int argc, char **argv) {
  if (argc < 3) {
    std::fprintf(stderr, "usage: %s <corpus dir> <seconds> [seed]\n", argv[0]);
    return 2;
  }
  std::vector<std::vector<uint8_t>> seeds;
  for (const auto &e : std::filesystem::directory_iterator(argv[1])) {
    std::ifstream in(e.path(), std::ios::binary);
    seeds.emplace_back(std::istreambuf_iterator<char>(in), std::istreambuf_iterator<char>());
  }
  if (seeds.empty()) {
    seeds.push_back({0, 0});
  }
  for (const auto &s : seeds) {
    LLVMFuzzerTestOneInput(s.data(), s.size());
  }
  const double seconds = std::atof(argv[2]);
  const auto seed = static_cast<uint32_t>(argc > 3 ? std::strtoul(argv[3], nullptr, 10) : 1);
  im::fuzzing::Mutator m(seed);
  std::vector<std::vector<uint8_t>> pool = seeds;
  const auto deadline = std::chrono::steady_clock::now() + std::chrono::duration<double>(seconds);
  uint64_t runs = 0;
  std::size_t longest = 0;
  while (std::chrono::steady_clock::now() < deadline) {
    for (int k = 0; k < 256; ++k) {
      std::vector<uint8_t> input = m.Mutate(pool[runs % pool.size()], seeds);
      LLVMFuzzerTestOneInput(input.data(), input.size());
      longest = input.size() > longest ? input.size() : longest;
      // A rolling pool of mutants, so mutations stack up over generations.
      if (pool.size() < 4096) {
        pool.push_back(std::move(input));
      } else {
        pool[(runs * 2654435761u) % pool.size()] = std::move(input);
      }
      ++runs;
    }
  }
  std::printf("%llu runs in %.0f s over %zu seeds, longest input %zu bytes, no failure\n",
              static_cast<unsigned long long>(runs), seconds, seeds.size(), longest);
  return 0;
}
