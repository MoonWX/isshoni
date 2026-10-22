#include "gpu/geometry.h"

#include <algorithm>

namespace im::gpu {

Rotation RotationFromDxgi(uint32_t dxgi_mode_rotation) noexcept {
  switch (dxgi_mode_rotation) {
  case 2: // DXGI_MODE_ROTATION_ROTATE90
    return Rotation::k90;
  case 3: // DXGI_MODE_ROTATION_ROTATE180
    return Rotation::k180;
  case 4: // DXGI_MODE_ROTATION_ROTATE270
    return Rotation::k270;
  default: // DXGI_MODE_ROTATION_UNSPECIFIED, DXGI_MODE_ROTATION_IDENTITY
    return Rotation::kIdentity;
  }
}

video::Size Upright(video::Size region, Rotation r) noexcept {
  if (r == Rotation::k90 || r == Rotation::k270) {
    return video::Size{region.height, region.width};
  }
  return region;
}

Point UprightToRegion(Rotation r, Point u) noexcept {
  switch (r) {
  case Rotation::k90:
    return Point{u.y, 1.0 - u.x};
  case Rotation::k180:
    return Point{1.0 - u.x, 1.0 - u.y};
  case Rotation::k270:
    return Point{1.0 - u.y, u.x};
  case Rotation::kIdentity:
    break;
  }
  return u;
}

Point UprightToRegionPixels(Rotation r, video::Size region, Point p) noexcept {
  const auto w = static_cast<double>(region.width);
  const auto h = static_cast<double>(region.height);
  switch (r) {
  case Rotation::k90: // the upright picture is h wide and w tall
    return Point{p.y, h - p.x};
  case Rotation::k180:
    return Point{w - p.x, h - p.y};
  case Rotation::k270:
    return Point{w - p.y, p.x};
  case Rotation::kIdentity:
    break;
  }
  return p;
}

ConvertGeometry ComputeGeometry(const video::OutputSize &layer, video::Size layer_source, video::Size texture,
                                video::Size content, Rotation rotation) noexcept {
  ConvertGeometry g;
  g.output = layer.size;
  g.texture = texture;
  g.content = video::Size{std::min(content.width, texture.width), std::min(content.height, texture.height)};
  g.rotation = rotation;
  const video::Size upright = Upright(g.content, rotation);
  if (upright.width == 0 || upright.height == 0 || g.output.width == 0 || g.output.height == 0) {
    g.dest = video::Rect{};
    return g;
  }
  g.dest = upright == layer_source ? layer.content : video::FitRect(upright, g.output);
  if (g.dest.width != 0 && g.dest.height != 0 && uint64_t{upright.width} >= 2 * uint64_t{g.dest.width} &&
      uint64_t{upright.height} >= 2 * uint64_t{g.dest.height}) {
    g.filter = Filter::kBox;
  }
  return g;
}

ConvertConstants MakeConvertConstants(const ConvertGeometry &g) noexcept {
  ConvertConstants c{};
  // A zero-size dest leaves every pixel outside it (black); the shader never divides by its size then.
  c.dest[0] = static_cast<float>(g.dest.x);
  c.dest[1] = static_cast<float>(g.dest.y);
  c.dest[2] = static_cast<float>(g.dest.width);
  c.dest[3] = static_cast<float>(g.dest.height);
  if (g.texture.width != 0 && g.texture.height != 0) {
    const auto tw = static_cast<double>(g.texture.width);
    const auto th = static_cast<double>(g.texture.height);
    c.src_scale[0] = static_cast<float>(static_cast<double>(g.content.width) / tw);
    c.src_scale[1] = static_cast<float>(static_cast<double>(g.content.height) / th);
    c.texel[0] = static_cast<float>(1.0 / tw);
    c.texel[1] = static_cast<float>(1.0 / th);
  }
  c.rotation = static_cast<uint32_t>(g.rotation);
  c.box = g.filter == Filter::kBox ? 1u : 0u;
  return c;
}

} // namespace im::gpu
