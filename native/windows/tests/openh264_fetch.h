// openh264_test's own support (docs/m2/04-windows-engine.md §18.2; README WA51): what the test needs to fetch Cisco's
// DLL itself and check the pinned hash, since the engine never downloads or hashes it (Go's InstallOpenH264 does, for
// the app) and the test executables have no Go: a bzip2 decompressor with an output limit and SHA-256. Header-only,
// portable C++20 with no OS header, so it is checked on the Mac too (README WA51's scratch run decompressed Cisco's
// real .bz2 with it and compared the hash). Only openh264_test.cpp includes it (README §5, "Shared files": a test's
// support header is the test's).
//
// The decompressor follows the bzip2 file format (Julian Seward's bzip2 1.0.x, documented in "BZip2 format
// specification", Joe Tsai, 2016): streams of "BZh1".."BZh9", blocks with the 48-bit magic 0x314159265359, the
// Huffman-coded MTF/RLE2 symbols, the inverse BWT, the RLE1 run lengths, the per-block and combined CRCs, and
// concatenated streams. Randomized blocks (deprecated since bzip2 0.9.5) are refused.
#pragma once

#include <algorithm>
#include <array>
#include <cstddef>
#include <cstdint>
#include <span>
#include <string>
#include <vector>

namespace im::openh264_test {

// ---------------------------------------------------------------------------------------------------- SHA-256

class Sha256 {
public:
  Sha256() noexcept { Reset(); }

  void Reset() noexcept {
    h_ = {0x6a09e667u, 0xbb67ae85u, 0x3c6ef372u, 0xa54ff53au, 0x510e527fu, 0x9b05688cu, 0x1f83d9abu, 0x5be0cd19u};
    len_ = 0;
    fill_ = 0;
  }

  void Update(std::span<const uint8_t> data) noexcept {
    for (const uint8_t b : data) {
      buf_[fill_++] = b;
      if (fill_ == 64) {
        Block(buf_.data());
        fill_ = 0;
      }
    }
    len_ += data.size();
  }

  // The digest as lower-case hex; the object must be Reset before it is used again.
  std::string HexDigest() noexcept {
    const uint64_t bits = len_ * 8;
    const uint8_t one = 0x80;
    Update(std::span<const uint8_t>(&one, 1));
    const uint8_t zero = 0;
    while (fill_ != 56) {
      Update(std::span<const uint8_t>(&zero, 1));
    }
    std::array<uint8_t, 8> l{};
    for (int i = 0; i < 8; ++i) {
      l[static_cast<std::size_t>(i)] = static_cast<uint8_t>(bits >> (56 - 8 * i));
    }
    Update(l);
    static constexpr char kHex[] = "0123456789abcdef";
    std::string out;
    for (const uint32_t w : h_) {
      for (int i = 3; i >= 0; --i) {
        const uint8_t b = static_cast<uint8_t>(w >> (8 * i));
        out.push_back(kHex[b >> 4]);
        out.push_back(kHex[b & 0xF]);
      }
    }
    return out;
  }

  static std::string Of(std::span<const uint8_t> data) {
    Sha256 s;
    s.Update(data);
    return s.HexDigest();
  }

private:
  static constexpr uint32_t Rotr(uint32_t x, int n) noexcept { return (x >> n) | (x << (32 - n)); }

  void Block(const uint8_t *p) noexcept {
    static constexpr std::array<uint32_t, 64> kK = {
        0x428a2f98u, 0x71374491u, 0xb5c0fbcfu, 0xe9b5dba5u, 0x3956c25bu, 0x59f111f1u, 0x923f82a4u, 0xab1c5ed5u,
        0xd807aa98u, 0x12835b01u, 0x243185beu, 0x550c7dc3u, 0x72be5d74u, 0x80deb1feu, 0x9bdc06a7u, 0xc19bf174u,
        0xe49b69c1u, 0xefbe4786u, 0x0fc19dc6u, 0x240ca1ccu, 0x2de92c6fu, 0x4a7484aau, 0x5cb0a9dcu, 0x76f988dau,
        0x983e5152u, 0xa831c66du, 0xb00327c8u, 0xbf597fc7u, 0xc6e00bf3u, 0xd5a79147u, 0x06ca6351u, 0x14292967u,
        0x27b70a85u, 0x2e1b2138u, 0x4d2c6dfcu, 0x53380d13u, 0x650a7354u, 0x766a0abbu, 0x81c2c92eu, 0x92722c85u,
        0xa2bfe8a1u, 0xa81a664bu, 0xc24b8b70u, 0xc76c51a3u, 0xd192e819u, 0xd6990624u, 0xf40e3585u, 0x106aa070u,
        0x19a4c116u, 0x1e376c08u, 0x2748774cu, 0x34b0bcb5u, 0x391c0cb3u, 0x4ed8aa4au, 0x5b9cca4fu, 0x682e6ff3u,
        0x748f82eeu, 0x78a5636fu, 0x84c87814u, 0x8cc70208u, 0x90befffau, 0xa4506cebu, 0xbef9a3f7u, 0xc67178f2u};
    std::array<uint32_t, 64> w{};
    for (std::size_t i = 0; i < 16; ++i) {
      w[i] = (uint32_t{p[4 * i]} << 24) | (uint32_t{p[4 * i + 1]} << 16) | (uint32_t{p[4 * i + 2]} << 8) |
             uint32_t{p[4 * i + 3]};
    }
    for (std::size_t i = 16; i < 64; ++i) {
      const uint32_t s0 = Rotr(w[i - 15], 7) ^ Rotr(w[i - 15], 18) ^ (w[i - 15] >> 3);
      const uint32_t s1 = Rotr(w[i - 2], 17) ^ Rotr(w[i - 2], 19) ^ (w[i - 2] >> 10);
      w[i] = w[i - 16] + s0 + w[i - 7] + s1;
    }
    uint32_t a = h_[0], b = h_[1], c = h_[2], d = h_[3], e = h_[4], f = h_[5], g = h_[6], h = h_[7];
    for (std::size_t i = 0; i < 64; ++i) {
      const uint32_t s1 = Rotr(e, 6) ^ Rotr(e, 11) ^ Rotr(e, 25);
      const uint32_t ch = (e & f) ^ (~e & g);
      const uint32_t t1 = h + s1 + ch + kK[i] + w[i];
      const uint32_t s0 = Rotr(a, 2) ^ Rotr(a, 13) ^ Rotr(a, 22);
      const uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
      const uint32_t t2 = s0 + maj;
      h = g;
      g = f;
      f = e;
      e = d + t1;
      d = c;
      c = b;
      b = a;
      a = t1 + t2;
    }
    h_[0] += a;
    h_[1] += b;
    h_[2] += c;
    h_[3] += d;
    h_[4] += e;
    h_[5] += f;
    h_[6] += g;
    h_[7] += h;
  }

  std::array<uint32_t, 8> h_{};
  std::array<uint8_t, 64> buf_{};
  std::size_t fill_ = 0;
  uint64_t len_ = 0;
};

// ---------------------------------------------------------------------------------------------------- bzip2

enum class Bzip2Result {
  kOk,
  kCorrupt,  // not bzip2, a damaged stream, a CRC mismatch, a randomized block
  kTooLarge, // more than max_out bytes would come out (decoding stops there)
};

namespace bzip2_detail {

class BitReader {
public:
  explicit BitReader(std::span<const uint8_t> in) noexcept : in_(in) {}

  // n ≤ 32 bits, most significant first; false past the end.
  bool Bits(int n, uint32_t *out) noexcept {
    uint32_t v = 0;
    for (int i = 0; i < n; ++i) {
      if (pos_ >= in_.size() * 8) {
        return false;
      }
      const uint8_t byte = in_[pos_ / 8];
      v = (v << 1) | ((byte >> (7 - pos_ % 8)) & 1u);
      ++pos_;
    }
    *out = v;
    return true;
  }
  bool Bit(uint32_t *out) noexcept { return Bits(1, out); }
  void AlignToByte() noexcept { pos_ = (pos_ + 7) / 8 * 8; }
  std::size_t BytePos() const noexcept { return pos_ / 8; }
  bool AtEnd() const noexcept { return pos_ >= in_.size() * 8; }

private:
  std::span<const uint8_t> in_;
  std::size_t pos_ = 0;
};

// bzip2's CRC-32: polynomial 0x04C11DB7, most significant bit first, initial and final value all ones.
inline uint32_t CrcUpdate(uint32_t crc, uint8_t b) noexcept {
  crc ^= uint32_t{b} << 24;
  for (int i = 0; i < 8; ++i) {
    crc = (crc & 0x80000000u) != 0 ? (crc << 1) ^ 0x04C11DB7u : crc << 1;
  }
  return crc;
}

// A canonical Huffman table (bzip2 assigns codes by length, then by symbol).
struct Huffman {
  std::array<int, 21> count{};  // codes per length 1..20
  std::array<int, 21> first{};  // the first code of each length
  std::array<int, 21> offset{}; // index into sorted of each length's first symbol
  std::array<uint16_t, 258> sorted{};

  bool Build(const uint8_t *lengths, int n) noexcept {
    count.fill(0);
    for (int i = 0; i < n; ++i) {
      if (lengths[i] < 1 || lengths[i] > 20) {
        return false;
      }
      ++count[lengths[i]];
    }
    int code = 0;
    int index = 0;
    for (int len = 1; len <= 20; ++len) {
      first[static_cast<std::size_t>(len)] = code;
      offset[static_cast<std::size_t>(len)] = index;
      code = (code + count[static_cast<std::size_t>(len)]) << 1;
      index += count[static_cast<std::size_t>(len)];
    }
    std::array<int, 21> next = offset;
    for (int i = 0; i < n; ++i) {
      sorted[static_cast<std::size_t>(next[lengths[i]]++)] = static_cast<uint16_t>(i);
    }
    return true;
  }

  bool Decode(BitReader &br, int *sym) const noexcept {
    int code = 0;
    for (std::size_t len = 1; len <= 20; ++len) {
      uint32_t bit = 0;
      if (!br.Bit(&bit)) {
        return false;
      }
      code = (code << 1) | static_cast<int>(bit);
      const int k = code - first[len];
      if (count[len] > 0 && k >= 0 && k < count[len]) {
        *sym = sorted[static_cast<std::size_t>(offset[len] + k)];
        return true;
      }
    }
    return false;
  }
};

// Decodes one block (after its magic) and appends its bytes to out; *crc is the block's computed CRC.
inline Bzip2Result Block(BitReader &br, uint32_t max_block, std::vector<uint8_t> *out, uint64_t max_out,
                         uint32_t *crc_out) {
  uint32_t stored_crc = 0;
  uint32_t randomized = 0;
  uint32_t orig_ptr = 0;
  if (!br.Bits(32, &stored_crc) || !br.Bit(&randomized) || !br.Bits(24, &orig_ptr) || randomized != 0) {
    return Bzip2Result::kCorrupt;
  }
  // The symbols in use.
  uint32_t used16 = 0;
  if (!br.Bits(16, &used16)) {
    return Bzip2Result::kCorrupt;
  }
  std::array<uint8_t, 256> seq_to_unseq{};
  int in_use = 0;
  for (int i = 0; i < 16; ++i) {
    if (((used16 >> (15 - i)) & 1u) == 0) {
      continue;
    }
    uint32_t bits = 0;
    if (!br.Bits(16, &bits)) {
      return Bzip2Result::kCorrupt;
    }
    for (int j = 0; j < 16; ++j) {
      if (((bits >> (15 - j)) & 1u) != 0) {
        seq_to_unseq[static_cast<std::size_t>(in_use++)] = static_cast<uint8_t>(i * 16 + j);
      }
    }
  }
  if (in_use == 0) {
    return Bzip2Result::kCorrupt;
  }
  const int alpha = in_use + 2;
  uint32_t n_groups = 0;
  uint32_t n_selectors = 0;
  if (!br.Bits(3, &n_groups) || n_groups < 2 || n_groups > 6 || !br.Bits(15, &n_selectors) || n_selectors == 0) {
    return Bzip2Result::kCorrupt;
  }
  // Selectors, MTF-coded (bzip2 1.0.8 reads up to 32767 and uses the first 18001).
  std::vector<uint8_t> selectors(n_selectors);
  std::array<uint8_t, 6> mtf_groups{0, 1, 2, 3, 4, 5};
  for (uint32_t i = 0; i < n_selectors; ++i) {
    uint32_t j = 0;
    for (;;) {
      uint32_t bit = 0;
      if (!br.Bit(&bit)) {
        return Bzip2Result::kCorrupt;
      }
      if (bit == 0) {
        break;
      }
      if (++j >= n_groups) {
        return Bzip2Result::kCorrupt;
      }
    }
    const uint8_t v = mtf_groups[j];
    for (uint32_t k = j; k > 0; --k) {
      mtf_groups[k] = mtf_groups[k - 1];
    }
    mtf_groups[0] = v;
    selectors[i] = v;
  }
  // The code lengths, delta-coded per table.
  std::array<Huffman, 6> tables;
  for (uint32_t t = 0; t < n_groups; ++t) {
    uint32_t len = 0;
    if (!br.Bits(5, &len)) {
      return Bzip2Result::kCorrupt;
    }
    std::array<uint8_t, 258> lengths{};
    for (int s = 0; s < alpha; ++s) {
      for (;;) {
        if (len < 1 || len > 20) {
          return Bzip2Result::kCorrupt;
        }
        uint32_t bit = 0;
        if (!br.Bit(&bit)) {
          return Bzip2Result::kCorrupt;
        }
        if (bit == 0) {
          break;
        }
        if (!br.Bit(&bit)) {
          return Bzip2Result::kCorrupt;
        }
        len = bit == 0 ? len + 1 : len - 1;
      }
      lengths[static_cast<std::size_t>(s)] = static_cast<uint8_t>(len);
    }
    if (!tables[t].Build(lengths.data(), alpha)) {
      return Bzip2Result::kCorrupt;
    }
  }
  // The symbols: MTF indexes with RUNA/RUNB runs of index 0, until EOB.
  std::vector<uint8_t> block;
  block.reserve(max_block);
  std::array<uint8_t, 256> mtf{};
  for (int i = 0; i < 256; ++i) {
    mtf[static_cast<std::size_t>(i)] = static_cast<uint8_t>(i);
  }
  std::array<uint32_t, 256> counts{};
  uint32_t group = 0;
  int left_in_group = 0;
  uint64_t run = 0;
  uint64_t run_weight = 1;
  const int eob = in_use + 1;
  const auto flush_run = [&]() -> bool {
    if (run == 0) {
      return true;
    }
    if (block.size() + run > max_block) {
      return false;
    }
    const uint8_t b = seq_to_unseq[mtf[0]];
    block.insert(block.end(), static_cast<std::size_t>(run), b);
    counts[b] += static_cast<uint32_t>(run);
    run = 0;
    run_weight = 1;
    return true;
  };
  for (;;) {
    if (left_in_group == 0) {
      if (group >= n_selectors) {
        return Bzip2Result::kCorrupt;
      }
      left_in_group = 50;
      ++group;
    }
    const Huffman &table = tables[selectors[group - 1]];
    --left_in_group;
    int sym = 0;
    if (!table.Decode(br, &sym)) {
      return Bzip2Result::kCorrupt;
    }
    if (sym <= 1) { // RUNA, RUNB: bijective base 2
      run += run_weight << sym;
      run_weight <<= 1;
      if (run > max_block) {
        return Bzip2Result::kCorrupt;
      }
      continue;
    }
    if (!flush_run()) {
      return Bzip2Result::kCorrupt;
    }
    if (sym == eob) {
      break;
    }
    const int idx = sym - 1;
    if (idx >= in_use) {
      return Bzip2Result::kCorrupt;
    }
    const uint8_t v = mtf[static_cast<std::size_t>(idx)];
    for (int k = idx; k > 0; --k) {
      mtf[static_cast<std::size_t>(k)] = mtf[static_cast<std::size_t>(k - 1)];
    }
    mtf[0] = v;
    if (block.size() >= max_block) {
      return Bzip2Result::kCorrupt;
    }
    const uint8_t b = seq_to_unseq[v];
    block.push_back(b);
    ++counts[b];
  }
  if (block.empty() || orig_ptr >= block.size()) {
    return Bzip2Result::kCorrupt;
  }
  // The inverse BWT.
  std::array<uint32_t, 256> cf{};
  uint32_t sum = 0;
  for (std::size_t i = 0; i < 256; ++i) {
    cf[i] = sum;
    sum += counts[i];
  }
  std::vector<uint32_t> tt(block.size());
  for (uint32_t i = 0; i < block.size(); ++i) {
    tt[cf[block[i]]++] = i;
  }
  // The RLE1 runs: after 4 equal bytes, a count of 0..255 more.
  uint32_t crc = 0xFFFFFFFFu;
  uint32_t pos = tt[orig_ptr];
  int same = 0;
  int last = -1;
  const auto emit = [&](uint8_t b) -> bool {
    if (out->size() >= max_out) {
      return false;
    }
    out->push_back(b);
    crc = CrcUpdate(crc, b);
    return true;
  };
  for (std::size_t i = 0; i < block.size(); ++i) {
    const uint8_t b = block[pos];
    pos = tt[pos];
    if (same == 4) {
      for (uint8_t k = 0; k < b; ++k) {
        if (!emit(static_cast<uint8_t>(last))) {
          return Bzip2Result::kTooLarge;
        }
      }
      same = 0;
      last = -1;
      continue;
    }
    if (b == last) {
      ++same;
    } else {
      same = 1;
      last = b;
    }
    if (!emit(b)) {
      return Bzip2Result::kTooLarge;
    }
  }
  crc = ~crc;
  if (crc != stored_crc) {
    return Bzip2Result::kCorrupt;
  }
  *crc_out = crc;
  return Bzip2Result::kOk;
}

} // namespace bzip2_detail

// Decompresses in (one or more concatenated bzip2 streams) into *out, at most max_out bytes: kTooLarge as soon as
// more would come out, with *out holding max_out bytes.
inline Bzip2Result Bzip2Decompress(std::span<const uint8_t> in, std::vector<uint8_t> *out, uint64_t max_out) {
  using namespace bzip2_detail;
  out->clear();
  BitReader br(in);
  bool any = false;
  while (!br.AtEnd()) {
    uint32_t magic = 0;
    uint32_t level = 0;
    if (!br.Bits(24, &magic) || magic != 0x425A68 /* "BZh" */ || !br.Bits(8, &level) || level < '1' || level > '9') {
      return Bzip2Result::kCorrupt;
    }
    const uint32_t max_block = (level - '0') * 100000;
    uint32_t combined = 0;
    for (;;) {
      uint32_t hi = 0;
      uint32_t lo = 0;
      if (!br.Bits(24, &hi) || !br.Bits(24, &lo)) {
        return Bzip2Result::kCorrupt;
      }
      if (hi == 0x314159 && lo == 0x265359) {
        uint32_t crc = 0;
        if (const Bzip2Result r = Block(br, max_block, out, max_out, &crc); r != Bzip2Result::kOk) {
          return r;
        }
        combined = ((combined << 1) | (combined >> 31)) ^ crc;
        continue;
      }
      if (hi == 0x177245 && lo == 0x385090) {
        uint32_t stored = 0;
        if (!br.Bits(32, &stored) || stored != combined) {
          return Bzip2Result::kCorrupt;
        }
        br.AlignToByte();
        break;
      }
      return Bzip2Result::kCorrupt;
    }
    any = true;
  }
  return any ? Bzip2Result::kOk : Bzip2Result::kCorrupt;
}

} // namespace im::openh264_test
