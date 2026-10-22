// Portable helpers of gpu_warp_test (docs/m2/04-windows-engine.md §18.2, README WA33): the golden picture format of
// testdata/gpu/, the test images, pointer shapes from pictures, the CPU model of the pointer blend and the NV12
// comparisons. No OS header, so the goldens can be checked against the CPU models (and written) on any machine.
#pragma once

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <fstream>
#include <map>
#include <sstream>
#include <string>
#include <string_view>
#include <vector>

#include "gpu/cpu_model.h"
#include "gpu/cursor_shape.h"
#include "gpu/geometry.h"
#include "im/video/scale.h"

namespace im::gpu::warptest {

using model::BgraImage;
using model::Nv12Image;

// ---------------------------------------------------------------------------------------------- golden pictures
//
// A picture is text, one character per pixel, or per n × n block with "scale n":
//
//   # comment lines (anywhere before "pixels")
//   size <width> <height>
//   scale <n>                optional (default 1); width and height are multiples of n
//   palette
//   <char> <AARRGGBB>        one color per line, hex; any printable character but '#' and space
//   pixels
//   <height / n lines of width / n characters>
//
// "pixels-hex" instead of "palette" … "pixels" holds height lines of width AARRGGBB words (for pictures with too many
// colors for a palette). Carriage returns are ignored, so a CRLF checkout (core.autocrlf on Windows) reads the same.

inline bool ParsePicture(std::string_view text, BgraImage *out, std::string *error) {
  std::vector<std::string> lines;
  {
    std::string line;
    for (char c : text) {
      if (c == '\r') {
        continue;
      }
      if (c == '\n') {
        lines.push_back(line);
        line.clear();
      } else {
        line.push_back(c);
      }
    }
    if (!line.empty()) {
      lines.push_back(line);
    }
  }
  uint32_t w = 0;
  uint32_t h = 0;
  uint32_t scale = 1;
  std::map<char, uint32_t> palette;
  enum class Section { kHeader, kPalette } section = Section::kHeader;
  std::size_t i = 0;
  bool hex = false;
  for (; i < lines.size(); ++i) {
    const std::string &l = lines[i];
    if (l.empty() || (section == Section::kHeader && l[0] == '#')) {
      continue;
    }
    std::istringstream in(l);
    std::string word;
    in >> word;
    if (section == Section::kPalette && word != "pixels") {
      std::string value;
      in >> value;
      if (word.size() != 1 || value.size() != 8 || word[0] == '#') {
        *error = "bad palette line: " + l;
        return false;
      }
      palette[word[0]] = static_cast<uint32_t>(std::stoul(value, nullptr, 16));
      continue;
    }
    if (word == "size") {
      in >> w >> h;
    } else if (word == "scale") {
      in >> scale;
    } else if (word == "palette") {
      section = Section::kPalette;
    } else if (word == "pixels") {
      ++i;
      break;
    } else if (word == "pixels-hex") {
      hex = true;
      ++i;
      break;
    } else {
      *error = "unknown line: " + l;
      return false;
    }
  }
  if (w == 0 || h == 0 || scale == 0 || w % scale != 0 || h % scale != 0) {
    *error = "bad size or scale";
    return false;
  }
  *out = BgraImage(w, h);
  if (hex) {
    for (uint32_t y = 0; y < h; ++y, ++i) {
      if (i >= lines.size()) {
        *error = "too few rows";
        return false;
      }
      std::istringstream in(lines[i]);
      for (uint32_t x = 0; x < w; ++x) {
        std::string v;
        if (!(in >> v) || v.size() != 8) {
          *error = "short hex row " + std::to_string(y);
          return false;
        }
        out->Set(x, y, static_cast<uint32_t>(std::stoul(v, nullptr, 16)));
      }
    }
    return true;
  }
  for (uint32_t by = 0; by < h / scale; ++by, ++i) {
    if (i >= lines.size() || lines[i].size() != w / scale) {
      *error = "row " + std::to_string(by) + " is not " + std::to_string(w / scale) + " characters";
      return false;
    }
    for (uint32_t bx = 0; bx < w / scale; ++bx) {
      const auto it = palette.find(lines[i][bx]);
      if (it == palette.end()) {
        *error = std::string("no palette entry for '") + lines[i][bx] + "'";
        return false;
      }
      for (uint32_t y = by * scale; y < (by + 1) * scale; ++y) {
        for (uint32_t x = bx * scale; x < (bx + 1) * scale; ++x) {
          out->Set(x, y, it->second);
        }
      }
    }
  }
  return true;
}

inline bool LoadPicture(const std::string &path, BgraImage *out, std::string *error) {
  std::ifstream f(path, std::ios::binary);
  if (!f) {
    *error = "can't open " + path;
    return false;
  }
  std::ostringstream s;
  s << f.rdbuf();
  if (!ParsePicture(s.str(), out, error)) {
    *error = path + ": " + *error;
    return false;
  }
  return true;
}

// The picture as text: with a palette when it has few colors, else as pixels-hex.
inline std::string FormatPicture(const BgraImage &img, std::string_view comment) {
  static constexpr std::string_view kChars =
      ".abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-*/=!?@$%&<>()[]{}^~:;,'|_";
  std::map<uint32_t, char> colors;
  bool too_many = false;
  for (uint32_t y = 0; y < img.height && !too_many; ++y) {
    for (uint32_t x = 0; x < img.width; ++x) {
      const uint32_t c = img.Get(x, y);
      if (colors.count(c) != 0) {
        continue;
      }
      if (colors.size() == kChars.size()) {
        too_many = true;
        break;
      }
      colors.emplace(c, kChars[colors.size()]);
    }
  }
  std::string out;
  for (std::size_t p = 0; p < comment.size();) {
    const std::size_t e = std::min(comment.find('\n', p), comment.size());
    out += "# " + std::string(comment.substr(p, e - p)) + "\n";
    p = e + 1;
  }
  out += "size " + std::to_string(img.width) + " " + std::to_string(img.height) + "\n";
  char hexbuf[16];
  if (too_many) {
    out += "pixels-hex\n";
    for (uint32_t y = 0; y < img.height; ++y) {
      for (uint32_t x = 0; x < img.width; ++x) {
        std::snprintf(hexbuf, sizeof hexbuf, "%s%08X", x == 0 ? "" : " ", img.Get(x, y));
        out += hexbuf;
      }
      out += "\n";
    }
    return out;
  }
  out += "palette\n";
  for (const auto &[c, ch] : colors) {
    std::snprintf(hexbuf, sizeof hexbuf, " %08X\n", c);
    out += ch;
    out += hexbuf;
  }
  out += "pixels\n";
  for (uint32_t y = 0; y < img.height; ++y) {
    for (uint32_t x = 0; x < img.width; ++x) {
      out += colors[img.Get(x, y)];
    }
    out += "\n";
  }
  return out;
}

// ------------------------------------------------------------------------------------------------- test images

// A deterministic picture with flat areas and edges of every kind: big color bars, blocks of colors from a seeded
// generator, a horizontal ramp, a one-pixel checkerboard and noise. Any size ≥ 8 × 8.
inline BgraImage TestImage(uint32_t w, uint32_t h, uint32_t seed = 1) {
  BgraImage img(w, h);
  uint32_t s = seed * 2654435761u + 12345u;
  const auto next = [&s] {
    s = s * 1664525u + 1013904223u;
    return s >> 8;
  };
  static constexpr uint32_t kBars[8] = {0xFFFFFFFFu, 0xFFC0C000u, 0xFF00C0C0u, 0xFF00C000u,
                                        0xFFC000C0u, 0xFFC00000u, 0xFF0000C0u, 0xFF101010u};
  std::vector<uint32_t> blocks((w / 32 + 1) * (h / 32 + 1));
  for (uint32_t &b : blocks) {
    b = 0xFF000000u | (next() & 0xFFFFFFu);
  }
  for (uint32_t y = 0; y < h; ++y) {
    for (uint32_t x = 0; x < w; ++x) {
      uint32_t c = 0;
      if (y < h / 4) {
        c = kBars[std::min<uint32_t>(x * 8 / w, 7)];
      } else if (y < h / 2) {
        c = blocks[(y / 32) * (w / 32 + 1) + x / 32];
      } else if (y < h * 5 / 8) {
        const uint32_t v = x * 255 / (w - 1);
        c = 0xFF000000u | (v << 16) | ((255 - v) << 8) | (v / 2);
      } else if (y < h * 3 / 4) {
        c = ((x + y) & 1) != 0 ? 0xFFF0F0F0u : 0xFF202020u;
      } else if (y < h * 7 / 8) {
        c = 0xFF000000u | (next() & 0xFFFFFFu);
      } else {
        c = x < w / 2 ? 0xFF3060A0u : 0xFFA06030u; // two wide flat areas
      }
      img.Set(x, y, c);
    }
  }
  return img;
}

// The image inside a larger texture: the rest is filled with fill (a color the converters must never read).
inline BgraImage Embed(const BgraImage &img, uint32_t tw, uint32_t th, uint32_t fill) {
  BgraImage out(tw, th);
  out.Fill(fill);
  for (uint32_t y = 0; y < img.height && y < th; ++y) {
    for (uint32_t x = 0; x < img.width && x < tw; ++x) {
      out.Set(x, y, img.Get(x, y));
    }
  }
  return out;
}

// --------------------------------------------------------------------------------------------- pointer shapes

// A pointer picture as Desktop Duplication delivers it (cursor_shape.h). Color and masked-color pictures are the
// buffer's BGRA (masked: alpha 0x00 or 0xFF is the mask); rows get `padding` extra bytes. A monochrome picture uses
// four colors for the mask bits: FF000000 AND 0 XOR 0, FFFFFFFF AND 0 XOR 1, 00000000 AND 1 XOR 0, 00FFFFFF AND 1
// XOR 1.
struct ShapeBuffer {
  PointerShape shape;
  std::vector<uint8_t> bytes;
};

inline ShapeBuffer ColorShape(const BgraImage &pic, uint32_t type, uint32_t padding = 8) {
  ShapeBuffer b;
  b.shape.type = type;
  b.shape.width = pic.width;
  b.shape.height = pic.height;
  b.shape.pitch = pic.width * 4 + padding;
  b.bytes.assign(static_cast<std::size_t>(b.shape.pitch) * pic.height, 0xCD);
  for (uint32_t y = 0; y < pic.height; ++y) {
    std::copy(pic.at(0, y), pic.at(0, y) + static_cast<std::size_t>(pic.width) * 4,
              b.bytes.begin() + static_cast<std::ptrdiff_t>(static_cast<std::size_t>(y) * b.shape.pitch));
  }
  return b;
}

inline ShapeBuffer MonochromeShape(const BgraImage &pic) {
  ShapeBuffer b;
  b.shape.type = kPointerMonochrome;
  b.shape.width = pic.width;
  b.shape.height = pic.height * 2;
  b.shape.pitch = (pic.width + 31) / 32 * 4;
  b.bytes.assign(static_cast<std::size_t>(b.shape.pitch) * b.shape.height, 0);
  for (uint32_t y = 0; y < pic.height; ++y) {
    for (uint32_t x = 0; x < pic.width; ++x) {
      const uint32_t c = pic.Get(x, y);
      const bool and_bit = (c >> 24) == 0;
      const bool xor_bit = (c & 0xFFFFFFu) != 0;
      const uint32_t bit = 0x80u >> (x % 8);
      if (and_bit) {
        uint8_t &byte = b.bytes[static_cast<std::size_t>(y) * b.shape.pitch + x / 8];
        byte = static_cast<uint8_t>(byte | bit);
      }
      if (xor_bit) {
        uint8_t &byte = b.bytes[static_cast<std::size_t>(y + pic.height) * b.shape.pitch + x / 8];
        byte = static_cast<uint8_t>(byte | bit);
      }
    }
  }
  return b;
}

inline BgraImage FromCursorImage(const CursorImage &c) {
  BgraImage img(c.width, c.height);
  for (uint32_t y = 0; y < c.height; ++y) {
    for (uint32_t x = 0; x < c.width; ++x) {
      img.Set(x, y, c.pixels[static_cast<std::size_t>(y) * c.width + x]);
    }
  }
  return img;
}

// The background of the pointer scenes: four quadrants (light grey, dark blue, white, dark green).
inline BgraImage Quadrants(uint32_t w, uint32_t h) {
  BgraImage img(w, h);
  for (uint32_t y = 0; y < h; ++y) {
    for (uint32_t x = 0; x < w; ++x) {
      const bool right = x >= w / 2;
      const bool bottom = y >= h / 2;
      img.Set(x, y, bottom ? (right ? 0xFF206020u : 0xFFFFFFFFu) : (right ? 0xFF202060u : 0xFFC0C0C0u));
    }
  }
  return img;
}

// A slot pixel (region coordinates, continuous) to the frame's upright coordinates: the inverse of
// UprightToRegionPixels, derived separately.
inline Point RegionToUpright(Rotation r, video::Size region, Point p) {
  const double w = region.width;
  const double h = region.height;
  switch (r) {
  case Rotation::k90: // region (Y, h − X)
    return Point{h - p.y, p.x};
  case Rotation::k180:
    return Point{w - p.x, h - p.y};
  case Rotation::k270: // region (w − Y, X)
    return Point{p.y, w - p.x};
  case Rotation::kIdentity:
    break;
  }
  return p;
}

// The CPU model of CursorOverlay::Blend: the pointer image with its top-left at upright (x, y), point-sampled, blended
// with straight alpha into every slot pixel whose center it covers; the slot's alpha stays.
inline void BlendModel(BgraImage *slot, const CursorImage &c, int32_t x, int32_t y, Rotation r) {
  const video::Size region{slot->width, slot->height};
  for (uint32_t py = 0; py < slot->height; ++py) {
    for (uint32_t px = 0; px < slot->width; ++px) {
      const Point u = RegionToUpright(r, region, Point{px + 0.5, py + 0.5});
      const double cx = u.x - x;
      const double cy = u.y - y;
      if (cx < 0 || cy < 0 || cx >= c.width || cy >= c.height) {
        continue;
      }
      const uint32_t s = c.pixels[static_cast<std::size_t>(cy) * c.width + static_cast<std::size_t>(cx)];
      const double a = static_cast<double>(s >> 24) / 255.0;
      const uint32_t d = slot->Get(px, py);
      uint32_t out = d & 0xFF000000u;
      for (int shift = 0; shift <= 16; shift += 8) {
        const double sc = static_cast<double>((s >> shift) & 0xFF);
        const double dc = static_cast<double>((d >> shift) & 0xFF);
        out |= static_cast<uint32_t>(model::Code(sc * a + dc * (1.0 - a))) << shift;
      }
      slot->Set(px, py, out);
    }
  }
}

// ------------------------------------------------------------------------------------------------- comparisons

// The largest differences between two NV12 pictures, split into flat and edge pixels by a classifier.
struct Nv12Diff {
  int max_flat = 0;
  int max_edge = 0;
  uint64_t flat = 0; // pixels compared
  uint64_t edge = 0;
  std::string worst; // where the largest difference is, for messages

  std::string Summary() const {
    return "flat max " + std::to_string(max_flat) + " (" + std::to_string(flat) + " px), edge max " +
           std::to_string(max_edge) + " (" + std::to_string(edge) + " px)" + (worst.empty() ? "" : "; worst " + worst);
  }
};

// classify(p) for a luma or chroma site p: 0 skip, 1 flat, 2 edge.
template <class Classify> Nv12Diff CompareNv12(const Nv12Image &got, const Nv12Image &want, Classify &&classify) {
  Nv12Diff d;
  int worst = -1;
  const auto note = [&](int diff, int kind, const std::string &where) {
    if (kind == 1) {
      ++d.flat;
      d.max_flat = std::max(d.max_flat, diff);
    } else {
      ++d.edge;
      d.max_edge = std::max(d.max_edge, diff);
    }
    if (diff > worst) {
      worst = diff;
      d.worst = where + " diff " + std::to_string(diff) + (kind == 1 ? " (flat)" : " (edge)");
    }
  };
  for (uint32_t y = 0; y < want.height; ++y) {
    for (uint32_t x = 0; x < want.width; ++x) {
      const int kind = classify(model::LumaSite(x, y));
      if (kind != 0) {
        note(std::abs(int{got.Y(x, y)} - int{want.Y(x, y)}), kind,
             "Y(" + std::to_string(x) + "," + std::to_string(y) + ") got " + std::to_string(got.Y(x, y)) + " want " +
                 std::to_string(want.Y(x, y)));
      }
    }
  }
  for (uint32_t j = 0; j < want.height / 2; ++j) {
    for (uint32_t i = 0; i < want.width / 2; ++i) {
      const int kind = classify(model::ChromaSite(i, j));
      if (kind == 0) {
        continue;
      }
      const int db = std::abs(int{got.Cb(i, j)} - int{want.Cb(i, j)});
      const int dr = std::abs(int{got.Cr(i, j)} - int{want.Cr(i, j)});
      note(std::max(db, dr), kind,
           "CbCr(" + std::to_string(i) + "," + std::to_string(j) + ") got " + std::to_string(got.Cb(i, j)) + "/" +
               std::to_string(got.Cr(i, j)) + " want " + std::to_string(want.Cb(i, j)) + "/" +
               std::to_string(want.Cr(i, j)));
    }
  }
  return d;
}

// The largest per-channel difference between two BGRA pictures of one size (alpha ignored), and where.
inline int MaxRgbDiff(const BgraImage &a, const BgraImage &b, std::string *where) {
  int worst = 0;
  for (uint32_t y = 0; y < a.height; ++y) {
    for (uint32_t x = 0; x < a.width; ++x) {
      const uint32_t p = a.Get(x, y);
      const uint32_t q = b.Get(x, y);
      for (int shift = 0; shift <= 16; shift += 8) {
        const int d = std::abs(static_cast<int>((p >> shift) & 0xFF) - static_cast<int>((q >> shift) & 0xFF));
        if (d > worst) {
          worst = d;
          char buf[96];
          std::snprintf(buf, sizeof buf, "(%u,%u) got %08X want %08X", x, y, p, q);
          *where = buf;
        }
      }
    }
  }
  return worst;
}

} // namespace im::gpu::warptest
