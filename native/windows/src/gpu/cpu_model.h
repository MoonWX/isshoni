// The CPU model of the converters (docs/m2/04-windows-engine.md §8.3): what the NV12 output of a frame should be.
//
// - Color: BT.709 Y'CbCr in limited range (Y 16..235, Cb/Cr 16..240) from the gamma-encoded R'G'B' of the BGRA
//   texture (no transfer change: input DXGI_COLOR_SPACE_RGB_FULL_G22_NONE_P709, output
//   DXGI_COLOR_SPACE_YCBCR_STUDIO_G22_LEFT_P709); outside the content rectangle black (Y 16, Cb/Cr 128).
// - Sampling: an output pixel's center maps into the content (geometry.h: rotation, scale); a tap is D3D11's LINEAR
//   filter with CLAMP addressing after the coordinates are clamped to the content's texel centers, so a content
//   smaller than its texture never reads past its edge; Filter::kBox averages four taps half a texel to each side.
// - Chroma: one Cb/Cr pair per 2×2 block, sited at the left luma column and between the two rows ("left" siting):
//   the chroma pixel (i, j) samples the luma position (2i + 0.5, 2j + 1), which at 1:1 is the mean of the block's
//   two left pixels.
//
// The shader converter implements exactly this (shaders/convert.hlsli). gpu_warp_test compares the shader on WARP
// with this model (≤ 1 code value on flat areas, ≤ 2 on edges) and the VideoProcessor with the shader. Header-only and
// portable; the engine itself never calls it.
#pragma once

#include <algorithm>
#include <cmath>
#include <cstddef>
#include <cstdint>
#include <limits>
#include <vector>

#include "gpu/geometry.h"

namespace im::gpu::model {

// BT.709's luma coefficients.
inline constexpr double kKr = 0.2126;
inline constexpr double kKb = 0.0722;
inline constexpr double kKg = 1.0 - kKr - kKb;

inline constexpr uint8_t kBlackY = 16;  // the bars
inline constexpr uint8_t kBlackC = 128; // Cb and Cr of the bars

// R'G'B' in [0, 1].
struct Rgb {
  double r = 0;
  double g = 0;
  double b = 0;
};

// Y'CbCr code values before rounding.
struct Ycc {
  double y = 0;
  double cb = 0;
  double cr = 0;
};

inline Ycc ToYcc(Rgb c) noexcept {
  const double y = kKr * c.r + kKg * c.g + kKb * c.b;
  return Ycc{16.0 + 219.0 * y, 128.0 + 224.0 * (c.b - y) / (2.0 * (1.0 - kKb)),
             128.0 + 224.0 * (c.r - y) / (2.0 * (1.0 - kKr))};
}

// Round to the nearest code value (what a float → UNORM8 render target write does).
inline uint8_t Code(double v) noexcept { return static_cast<uint8_t>(std::clamp(std::floor(v + 0.5), 0.0, 255.0)); }

// A BGRA picture: 4 bytes per pixel in memory order B, G, R, A; rows without padding.
struct BgraImage {
  uint32_t width = 0;
  uint32_t height = 0;
  std::vector<uint8_t> bgra;

  BgraImage() = default;
  BgraImage(uint32_t w, uint32_t h) : width(w), height(h), bgra(static_cast<std::size_t>(w) * h * 4, 0) {}

  const uint8_t *at(uint32_t x, uint32_t y) const noexcept {
    return bgra.data() + (static_cast<std::size_t>(y) * width + x) * 4;
  }
  uint8_t *at(uint32_t x, uint32_t y) noexcept { return bgra.data() + (static_cast<std::size_t>(y) * width + x) * 4; }
  // As 0xAARRGGBB.
  uint32_t Get(uint32_t x, uint32_t y) const noexcept {
    const uint8_t *p = at(x, y);
    return uint32_t{p[0]} | (uint32_t{p[1]} << 8) | (uint32_t{p[2]} << 16) | (uint32_t{p[3]} << 24);
  }
  void Set(uint32_t x, uint32_t y, uint32_t argb) noexcept {
    uint8_t *p = at(x, y);
    p[0] = static_cast<uint8_t>(argb);
    p[1] = static_cast<uint8_t>(argb >> 8);
    p[2] = static_cast<uint8_t>(argb >> 16);
    p[3] = static_cast<uint8_t>(argb >> 24);
  }
  void Fill(uint32_t argb) noexcept {
    for (uint32_t y = 0; y < height; ++y) {
      for (uint32_t x = 0; x < width; ++x) {
        Set(x, y, argb);
      }
    }
  }
};

// An NV12 picture in system memory: the Y plane (width × height) and the interleaved Cb/Cr plane
// (width/2 pairs × height/2 rows), rows without padding.
struct Nv12Image {
  uint32_t width = 0;
  uint32_t height = 0;
  std::vector<uint8_t> y;
  std::vector<uint8_t> uv;

  Nv12Image() = default;
  Nv12Image(uint32_t w, uint32_t h)
      : width(w), height(h), y(static_cast<std::size_t>(w) * h, 0), uv(static_cast<std::size_t>(w) * (h / 2), 0) {}

  uint8_t Y(uint32_t x, uint32_t yy) const noexcept { return y[static_cast<std::size_t>(yy) * width + x]; }
  uint8_t Cb(uint32_t i, uint32_t j) const noexcept { return uv[static_cast<std::size_t>(j) * width + 2 * i]; }
  uint8_t Cr(uint32_t i, uint32_t j) const noexcept { return uv[static_cast<std::size_t>(j) * width + 2 * i + 1]; }
};

// The luma-plane position a pixel of either plane samples: a luma pixel's center, or a chroma pixel's site.
inline Point LumaSite(uint32_t x, uint32_t y) noexcept { return Point{x + 0.5, y + 0.5}; }
inline Point ChromaSite(uint32_t i, uint32_t j) noexcept { return Point{2.0 * i + 0.5, 2.0 * j + 1.0}; }

inline bool InsideDest(const ConvertGeometry &g, Point p) noexcept {
  return p.x >= g.dest.x && p.y >= g.dest.y && p.x < static_cast<double>(g.dest.x) + g.dest.width &&
         p.y < static_cast<double>(g.dest.y) + g.dest.height;
}

// The texels one tap reads.
struct TexelBox {
  int64_t x0 = 0;
  int64_t y0 = 0;
  int64_t x1 = 0; // inclusive
  int64_t y1 = 0;
};

namespace detail {

// A tap's normalized coordinate, clamped to the content's texel centers (shaders/convert.hlsli's Tap).
inline double ClampToContent(double s, uint32_t content, uint32_t texture) noexcept {
  const double lo = 0.5 / texture;
  const double hi = (content - 0.5) / texture;
  return std::clamp(s, lo, std::max(lo, hi));
}

// The tap positions (normalized texture coordinates) of an output position inside dest.
inline int Taps(const ConvertGeometry &g, Point p, Point (&taps)[4]) noexcept {
  const Point u{(p.x - g.dest.x) / g.dest.width, (p.y - g.dest.y) / g.dest.height};
  const Point r = UprightToRegion(g.rotation, u);
  const double tw = g.texture.width;
  const double th = g.texture.height;
  const Point st{r.x * g.content.width / tw, r.y * g.content.height / th};
  if (g.filter != Filter::kBox) {
    taps[0] = st;
    return 1;
  }
  const double hx = 0.5 / tw;
  const double hy = 0.5 / th;
  taps[0] = Point{st.x - hx, st.y - hy};
  taps[1] = Point{st.x + hx, st.y - hy};
  taps[2] = Point{st.x - hx, st.y + hy};
  taps[3] = Point{st.x + hx, st.y + hy};
  return 4;
}

} // namespace detail

// One LINEAR tap with CLAMP addressing at normalized texture coordinates (s, t), the coordinates clamped to the
// content first.
inline Rgb Tap(const BgraImage &tex, video::Size content, double s, double t, TexelBox *box = nullptr) noexcept {
  s = detail::ClampToContent(s, content.width, tex.width);
  t = detail::ClampToContent(t, content.height, tex.height);
  const double x = s * tex.width - 0.5;
  const double y = t * tex.height - 0.5;
  const double fx0 = std::floor(x);
  const double fy0 = std::floor(y);
  const double fx = x - fx0;
  const double fy = y - fy0;
  const auto clampx = [&](double v) { return static_cast<uint32_t>(std::clamp(v, 0.0, tex.width - 1.0)); };
  const auto clampy = [&](double v) { return static_cast<uint32_t>(std::clamp(v, 0.0, tex.height - 1.0)); };
  const uint32_t x0 = clampx(fx0);
  const uint32_t x1 = clampx(fx0 + 1);
  const uint32_t y0 = clampy(fy0);
  const uint32_t y1 = clampy(fy0 + 1);
  if (box != nullptr) {
    // Texels with weight 0 are left out, so a tap exactly on a texel center reads that texel only.
    box->x0 = x0;
    box->x1 = fx > 0 ? x1 : x0;
    box->y0 = y0;
    box->y1 = fy > 0 ? y1 : y0;
  }
  Rgb out;
  const double w[4] = {(1 - fx) * (1 - fy), fx * (1 - fy), (1 - fx) * fy, fx * fy};
  const uint8_t *px[4] = {tex.at(x0, y0), tex.at(x1, y0), tex.at(x0, y1), tex.at(x1, y1)};
  for (int i = 0; i < 4; ++i) {
    out.b += w[i] * px[i][0] / 255.0;
    out.g += w[i] * px[i][1] / 255.0;
    out.r += w[i] * px[i][2] / 255.0;
  }
  return out;
}

// The color the converter computes at an output position p inside g.dest.
inline Rgb Fetch(const BgraImage &tex, const ConvertGeometry &g, Point p) noexcept {
  Point taps[4];
  const int n = detail::Taps(g, p, taps);
  Rgb sum;
  for (int i = 0; i < n; ++i) {
    const Rgb c = Tap(tex, g.content, taps[i].x, taps[i].y);
    sum.r += c.r;
    sum.g += c.g;
    sum.b += c.b;
  }
  return Rgb{sum.r / n, sum.g / n, sum.b / n};
}

// Whether every texel the output position p reads, grown by margin texels on each side (within the content), has
// one color: the "flat areas" of 04 §8.3. Positions outside dest are flat (black).
inline bool Flat(const BgraImage &tex, const ConvertGeometry &g, Point p, int margin) noexcept {
  if (!InsideDest(g, p)) {
    return true;
  }
  Point taps[4];
  const int n = detail::Taps(g, p, taps);
  constexpr int64_t kMax = std::numeric_limits<int64_t>::max();
  constexpr int64_t kMin = std::numeric_limits<int64_t>::min();
  TexelBox all{kMax, kMax, kMin, kMin};
  for (int i = 0; i < n; ++i) {
    TexelBox b;
    (void)Tap(tex, g.content, taps[i].x, taps[i].y, &b);
    all.x0 = std::min(all.x0, b.x0);
    all.y0 = std::min(all.y0, b.y0);
    all.x1 = std::max(all.x1, b.x1);
    all.y1 = std::max(all.y1, b.y1);
  }
  const int64_t x0 = std::max<int64_t>(all.x0 - margin, 0);
  const int64_t y0 = std::max<int64_t>(all.y0 - margin, 0);
  const int64_t x1 = std::min<int64_t>(all.x1 + margin, int64_t{g.content.width} - 1);
  const int64_t y1 = std::min<int64_t>(all.y1 + margin, int64_t{g.content.height} - 1);
  const uint32_t ref = tex.Get(static_cast<uint32_t>(x0), static_cast<uint32_t>(y0)) & 0x00FFFFFFu;
  for (int64_t y = y0; y <= y1; ++y) {
    for (int64_t x = x0; x <= x1; ++x) {
      if ((tex.Get(static_cast<uint32_t>(x), static_cast<uint32_t>(y)) & 0x00FFFFFFu) != ref) {
        return false;
      }
    }
  }
  return true;
}

// The NV12 picture the converters should produce for the content of tex.
inline Nv12Image Convert(const BgraImage &tex, const ConvertGeometry &g) {
  Nv12Image out(g.output.width, g.output.height);
  for (uint32_t y = 0; y < out.height; ++y) {
    for (uint32_t x = 0; x < out.width; ++x) {
      const Point p = LumaSite(x, y);
      out.y[static_cast<std::size_t>(y) * out.width + x] = InsideDest(g, p) ? Code(ToYcc(Fetch(tex, g, p)).y) : kBlackY;
    }
  }
  for (uint32_t j = 0; j < out.height / 2; ++j) {
    for (uint32_t i = 0; i < out.width / 2; ++i) {
      const Point p = ChromaSite(i, j);
      uint8_t cb = kBlackC;
      uint8_t cr = kBlackC;
      if (InsideDest(g, p)) {
        const Ycc c = ToYcc(Fetch(tex, g, p));
        cb = Code(c.cb);
        cr = Code(c.cr);
      }
      out.uv[static_cast<std::size_t>(j) * out.width + 2 * i] = cb;
      out.uv[static_cast<std::size_t>(j) * out.width + 2 * i + 1] = cr;
    }
  }
  return out;
}

} // namespace im::gpu::model
