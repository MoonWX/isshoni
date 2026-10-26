// gpu_warp_test (docs/m2/04-windows-engine.md §18.2, README WA33): the GPU path on WARP, the software rasterizer every
// Windows has (CI's windows-2025 runner has no GPU). It asserts:
//
// - the shader converter against the CPU BT.709 limited-range model (gpu/cpu_model.h): ≤ 1 code value on flat areas,
//   ≤ 2 on edges; at 1:1, downscaled (bilinear), as the preview (the 2×2 box), for an odd window, a content smaller
//   than its texture and rotated panels;
// - the VideoProcessor against the shader output (≤ 2 on flat areas) when WARP offers one, else skipped with the
//   reason; with the test hooks, a failed VideoProcessorBlt switching the layer to the shader, one VideoProcessor per
//   slot texture size (none created per frame when the slots alternate between sizes) and the first one made for
//   ConverterConfig::input rather than on the first frame;
// - the letterbox: geometry, bars of exactly Y 16 and Cb/Cr 128, the content against golden pictures;
// - rotation 90/180/270 against golden pictures;
// - pointer blending of color, masked-color and monochrome shapes against golden pictures (also cut at the frame's
//   edge and on rotated frames), once per frame, and nothing drawn with IM_START_HIDE_CURSOR;
// - the GPU test pattern renders: every pixel equals pattern_layout.h's model;
// - the frame slots (newest frame wins, the reader keeps its frame, growth in steps of 256), the NV12 readback (planar
//   sources checked; with the test hooks, the context lock free while it waits for the GPU), the context lock and the
//   device-loss mapping.
//
// The goldens are text pictures in testdata/gpu/ (format: gpu_warp_support.h). With IM_GPU_GOLDEN_OUT=<folder> the
// test writes what WARP produced there (pictures in the same format, NV12 planes as PGM) for review.
#include <gtest/gtest.h>

#include <windows.h>

#include <d3d11.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <future>
#include <memory>
#include <string>
#include <thread>
#include <vector>

#include "gpu/converter.h"
#include "gpu/cpu_model.h"
#include "gpu/cursor.h"
#include "gpu/cursor_shape.h"
#include "gpu/device.h"
#include "gpu/geometry.h"
#include "gpu/nv12.h"
#include "gpu/pattern_layout.h"
#include "gpu/slots.h"
#include "gpu/test_pattern.h"
#include "gpu/triple_buffer.h"
#include "gpu/video_processor.h"
#include "gpu_warp_support.h"
#include "im/abi/status.h"
#include "im/video/scale.h"
#include "isshoni_media.h"

namespace {

using im::gpu::ComPtr;
using im::gpu::ContextLock;
using im::gpu::Converter;
using im::gpu::ConverterConfig;
using im::gpu::ConvertGeometry;
using im::gpu::CursorImage;
using im::gpu::CursorOverlay;
using im::gpu::Device;
using im::gpu::DeviceRequest;
using im::gpu::FrameMeta;
using im::gpu::FrameSlots;
using im::gpu::Nv12Readback;
using im::gpu::Nv12Surface;
using im::gpu::Point;
using im::gpu::Rotation;
using im::gpu::Slot;
using im::gpu::SourceFrame;
using im::gpu::TestPattern;
using im::gpu::model::BgraImage;
using im::gpu::model::Nv12Image;
using im::video::LayerBounds;
using im::video::OutputSize;
using im::video::Rect;
using im::video::Size;
namespace model = im::gpu::model;
namespace wt = im::gpu::warptest;

constexpr uint32_t kMagenta = 0xFFFF00FFu; // texels outside a frame's content: never read

std::string Detail() { return std::string(im::abi::LastErrorText()); }

std::string Env(const char *name) {
  char buf[1024];
  const DWORD n = GetEnvironmentVariableA(name, buf, static_cast<DWORD>(sizeof buf));
  return n > 0 && n < sizeof buf ? std::string(buf, n) : std::string();
}

std::string TestdataPath(const char *name) { return std::string(IM_TESTDATA_DIR) + "/gpu/" + name; }

BgraImage Golden(const char *name) {
  BgraImage img;
  std::string err;
  EXPECT_TRUE(wt::LoadPicture(TestdataPath(name), &img, &err)) << err;
  return img;
}

// IM_GPU_GOLDEN_OUT: what the GPU produced, for review (and to update a golden after a reviewed change).
void WriteActual(const std::string &name, const BgraImage &img) {
  const std::string dir = Env("IM_GPU_GOLDEN_OUT");
  if (dir.empty()) {
    return;
  }
  std::ofstream f(dir + "/" + name, std::ios::binary);
  f << wt::FormatPicture(img, "written by gpu_warp_test (IM_GPU_GOLDEN_OUT): WARP's output");
}

void WriteActualNv12(const std::string &name, const Nv12Image &img) {
  const std::string dir = Env("IM_GPU_GOLDEN_OUT");
  if (dir.empty()) {
    return;
  }
  const auto pgm = [&](const std::string &file, const std::vector<uint8_t> &plane, uint32_t w, uint32_t h) {
    std::ofstream f(dir + "/" + file, std::ios::binary);
    f << "P5\n" << w << " " << h << "\n255\n";
    f.write(reinterpret_cast<const char *>(plane.data()), static_cast<std::streamsize>(plane.size()));
  };
  pgm(name + ".y.pgm", img.y, img.width, img.height);
  pgm(name + ".uv.pgm", img.uv, img.width, img.height / 2); // Cb, Cr interleaved
}

// ---------------------------------------------------------------------------------------------- GPU helpers

struct Texture {
  ComPtr<ID3D11Texture2D> tex;
  ComPtr<ID3D11ShaderResourceView> srv;
  uint64_t id = 0;
  Size size;

  SourceFrame Source(Size content, Rotation r) const { return SourceFrame{tex.Get(), srv.Get(), id, size, content, r}; }
};

// A BGRA texture (shader resource and render target, as a frame slot) holding img.
Texture Upload(Device &d, const BgraImage &img) {
  Texture t;
  D3D11_TEXTURE2D_DESC td{};
  td.Width = img.width;
  td.Height = img.height;
  td.MipLevels = 1;
  td.ArraySize = 1;
  td.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
  td.SampleDesc.Count = 1;
  td.Usage = D3D11_USAGE_DEFAULT;
  td.BindFlags = D3D11_BIND_SHADER_RESOURCE | D3D11_BIND_RENDER_TARGET;
  D3D11_SUBRESOURCE_DATA init{};
  init.pSysMem = img.bgra.data();
  init.SysMemPitch = img.width * 4;
  HRESULT hr = d.d3d()->CreateTexture2D(&td, &init, t.tex.GetAddressOf());
  if (SUCCEEDED(hr)) {
    hr = d.d3d()->CreateShaderResourceView(t.tex.Get(), nullptr, t.srv.GetAddressOf());
  }
  if (FAILED(hr)) {
    ADD_FAILURE() << "texture upload failed: 0x" << std::hex << static_cast<uint32_t>(hr);
    return Texture{};
  }
  t.id = im::gpu::NewTextureId();
  t.size = Size{img.width, img.height};
  return t;
}

// Writes img into the top-left of a slot's texture.
void FillSlot(Device &d, Slot &slot, const BgraImage &img) {
  D3D11_BOX box{0, 0, 0, img.width, img.height, 1};
  ContextLock lock(d);
  d.context()->UpdateSubresource(slot.texture.Get(), 0, &box, img.bgra.data(), img.width * 4, 0);
}

BgraImage ReadBgra(Device &d, ID3D11Texture2D *tex) {
  D3D11_TEXTURE2D_DESC td{};
  tex->GetDesc(&td);
  td.Usage = D3D11_USAGE_STAGING;
  td.BindFlags = 0;
  td.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
  td.MiscFlags = 0;
  ComPtr<ID3D11Texture2D> staging;
  if (FAILED(d.d3d()->CreateTexture2D(&td, nullptr, staging.GetAddressOf()))) {
    ADD_FAILURE() << "staging texture failed";
    return BgraImage{};
  }
  BgraImage out(td.Width, td.Height);
  ContextLock lock(d);
  d.context()->CopyResource(staging.Get(), tex);
  D3D11_MAPPED_SUBRESOURCE m{};
  if (FAILED(d.context()->Map(staging.Get(), 0, D3D11_MAP_READ, 0, &m))) {
    ADD_FAILURE() << "Map failed";
    return BgraImage{};
  }
  for (uint32_t y = 0; y < td.Height; ++y) {
    std::memcpy(out.at(0, y), static_cast<const uint8_t *>(m.pData) + static_cast<std::size_t>(y) * m.RowPitch,
                static_cast<std::size_t>(td.Width) * 4);
  }
  d.context()->Unmap(staging.Get(), 0);
  return out;
}

// Converts src with conv into a surface of the layer's size and reads it back.
Nv12Image ConvertAndRead(Device &d, Converter &conv, const SourceFrame &src) {
  const Size size = conv.config().output.size;
  std::unique_ptr<Nv12Surface> surface;
  if (Nv12Surface::Create(d, size, &surface) != IM_OK) {
    ADD_FAILURE() << "Nv12Surface: " << Detail();
    return Nv12Image{};
  }
  const im_status st = conv.Convert(src, surface->target());
  if (st != IM_OK) {
    ADD_FAILURE() << "Convert: " << im::abi::StatusName(st) << " " << Detail();
    return Nv12Image{};
  }
  std::unique_ptr<Nv12Readback> rb;
  if (Nv12Readback::Create(d, size, surface->planar(), &rb) != IM_OK) {
    ADD_FAILURE() << "Nv12Readback: " << Detail();
    return Nv12Image{};
  }
  Nv12Image out(size.width, size.height);
  const im::gpu::Nv12Planes planes{out.y.data(), size.width, out.uv.data(), size.width};
  if (rb->Read(surface->target(), planes) != IM_OK) {
    ADD_FAILURE() << "Read: " << Detail();
  }
  return out;
}

// input: the slot texture size in the capture's orientation (ConverterConfig::input); empty: upright.
std::unique_ptr<Converter> MakeConverter(Device &d, const OutputSize &layer, Size upright, bool video_processor,
                                         Size input = {}) {
  ConverterConfig cfg;
  cfg.output = layer;
  cfg.source = upright;
  cfg.input = input;
  cfg.fps = 60;
  cfg.video_processor = video_processor;
  std::unique_ptr<Converter> c;
  EXPECT_EQ(Converter::Create(d, cfg, &c), IM_OK) << Detail();
  return c;
}

// A 1:1 geometry of a picture onto itself (for converting an expected upright picture with the CPU model).
ConvertGeometry Identity(const BgraImage &img) {
  OutputSize out;
  out.size = Size{img.width, img.height};
  out.content = Rect{0, 0, img.width, img.height};
  return im::gpu::ComputeGeometry(out, out.size, out.size, out.size, Rotation::kIdentity);
}

// The distance (output pixels) from p to the edge of g.dest, negative outside it.
double DistanceToDestEdge(const ConvertGeometry &g, Point p) {
  const double l = p.x - g.dest.x;
  const double t = p.y - g.dest.y;
  const double r = g.dest.x + static_cast<double>(g.dest.width) - p.x;
  const double b = g.dest.y + static_cast<double>(g.dest.height) - p.y;
  const double inside = std::min(std::min(l, r), std::min(t, b));
  return inside;
}

const char *RotationName(Rotation r) {
  switch (r) {
  case Rotation::k90:
    return "90";
  case Rotation::k180:
    return "180";
  case Rotation::k270:
    return "270";
  case Rotation::kIdentity:
    break;
  }
  return "0";
}

// Whether dev offers a VideoProcessor for BGRA → BT.709 limited NV12 (and why not).
bool HasVideoProcessor(Device &dev, std::string *why) {
  std::unique_ptr<im::gpu::VideoProcessorConverter> vp;
  if (im::gpu::VideoProcessorConverter::Create(dev, Size{1920, 1080}, Size{1920, 1080}, 60, &vp) != IM_OK) {
    *why = Detail();
    im::abi::ClearLastError();
    return false;
  }
  return true;
}

class GpuWarp : public ::testing::Test {
protected:
  void SetUp() override {
    DeviceRequest r;
    r.adapter = DeviceRequest::Adapter::kWarp;
    ASSERT_EQ(Device::Create(r, &dev_), IM_OK) << Detail();
    ASSERT_TRUE(dev_->warp());
  }

  bool VideoProcessorAvailable(std::string *why) { return HasVideoProcessor(*dev_, why); }

  std::unique_ptr<Device> dev_;
};

// ------------------------------------------------------------------------------------- shader vs the CPU model

struct ConvertCase {
  const char *name;
  Size content; // the frame's content, in the capture's orientation
  Size texture; // its texture (larger: the rest is magenta)
  LayerBounds bounds;
  Rotation rotation;
};

const ConvertCase kConvertCases[] = {
    {"1080p 1:1", {1920, 1080}, {1920, 1080}, {}, Rotation::kIdentity},
    {"1440p to 1080p (bilinear)", {2560, 1440}, {2560, 1440}, {2073600, 0, 0}, Rotation::kIdentity},
    {"preview 640x360 of 1080p (box)", {1920, 1080}, {1920, 1080}, {230400, 0, 0}, Rotation::kIdentity},
    {"preview 2:1 of 720p (box)", {1280, 720}, {1280, 720}, {230400, 0, 0}, Rotation::kIdentity},
    {"odd window 1001x751", {1001, 751}, {1001, 751}, {}, Rotation::kIdentity},
    {"content inside a larger texture", {1000, 700}, {1280, 800}, {}, Rotation::kIdentity},
    {"panel turned 90, to 720p", {1080, 1920}, {1080, 1920}, {921600, 0, 0}, Rotation::k90},
    {"panel turned 270, 1:1", {720, 1280}, {720, 1280}, {}, Rotation::k270},
    {"panel turned 180, width 960", {1280, 720}, {1280, 720}, {0, 960, 0}, Rotation::k180},
};

TEST_F(GpuWarp, ShaderMatchesCpuModel) {
  uint32_t seed = 1;
  for (const ConvertCase &c : kConvertCases) {
    SCOPED_TRACE(c.name);
    const BgraImage tex_img = wt::Embed(wt::TestImage(c.content.width, c.content.height, seed++), c.texture.width,
                                        c.texture.height, kMagenta);
    const Texture tex = Upload(*dev_, tex_img);
    ASSERT_FALSE(HasFailure());
    const Size upright = im::gpu::Upright(c.content, c.rotation);
    const OutputSize layer = im::video::ComputeOutputSize(upright, c.bounds);
    const std::unique_ptr<Converter> conv = MakeConverter(*dev_, layer, upright, false);
    ASSERT_TRUE(conv);
    EXPECT_EQ(conv->path(), Converter::Path::kShader);
    const SourceFrame src = tex.Source(c.content, c.rotation);
    const ConvertGeometry g = conv->Geometry(src);
    EXPECT_TRUE(g.dest == layer.content);
    const Nv12Image got = ConvertAndRead(*dev_, *conv, src);
    ASSERT_FALSE(HasFailure());
    const Nv12Image want = model::Convert(tex_img, g);
    const wt::Nv12Diff d = wt::CompareNv12(got, want, [&](Point p) { return model::Flat(tex_img, g, p, 1) ? 1 : 2; });
    std::printf("[ info     ] shader vs model, %s (%ux%u, filter %s): %s\n", c.name, layer.size.width,
                layer.size.height, g.filter == im::gpu::Filter::kBox ? "box" : "bilinear", d.Summary().c_str());
    EXPECT_LE(d.max_flat, 1) << d.Summary();
    EXPECT_LE(d.max_edge, 2) << d.Summary();
    EXPECT_GT(d.flat, 0u);
    if (d.max_flat > 1 || d.max_edge > 2) {
      WriteActualNv12(std::string("shader_") + std::to_string(seed - 1), got);
    }
  }
}

TEST_F(GpuWarp, ConverterReconfigures) {
  const BgraImage img = wt::TestImage(1920, 1080, 42);
  const Texture tex = Upload(*dev_, img);
  ASSERT_FALSE(HasFailure());
  const Size source{1920, 1080};
  std::unique_ptr<Converter> conv = MakeConverter(*dev_, im::video::ComputeOutputSize(source, {}), source, false);
  ASSERT_TRUE(conv);
  // An fps or size re-init (04 §9.5): the layer's new output size.
  ConverterConfig cfg = conv->config();
  cfg.output = im::video::ComputeOutputSize(source, LayerBounds{921600, 0, 0});
  ASSERT_EQ(conv->Reconfigure(cfg), IM_OK);
  const SourceFrame src = tex.Source(source, Rotation::kIdentity);
  const ConvertGeometry g = conv->Geometry(src);
  EXPECT_TRUE(g.output == (Size{1280, 720}));
  const Nv12Image got = ConvertAndRead(*dev_, *conv, src);
  ASSERT_FALSE(HasFailure());
  const wt::Nv12Diff d =
      wt::CompareNv12(got, model::Convert(img, g), [&](Point p) { return model::Flat(img, g, p, 1) ? 1 : 2; });
  EXPECT_LE(d.max_flat, 1) << d.Summary();
  EXPECT_LE(d.max_edge, 2) << d.Summary();

  // A content of another size (a resized window before its re-init, 04 §7.7) is fitted into the output, black bars
  // around it, and never reads past its region of the texture.
  const SourceFrame fitted = tex.Source(Size{1000, 1000}, Rotation::kIdentity);
  const ConvertGeometry gs = conv->Geometry(fitted);
  EXPECT_TRUE(gs.dest == (Rect{280, 0, 720, 720}));
  const Nv12Image got_fitted = ConvertAndRead(*dev_, *conv, fitted);
  ASSERT_FALSE(HasFailure());
  const wt::Nv12Diff ds =
      wt::CompareNv12(got_fitted, model::Convert(img, gs), [&](Point p) { return model::Flat(img, gs, p, 1) ? 1 : 2; });
  EXPECT_LE(ds.max_flat, 1) << ds.Summary();
  EXPECT_LE(ds.max_edge, 2) << ds.Summary();
}

// ------------------------------------------------------------------------------ the VideoProcessor vs the shader

// The VideoProcessor against the shader converter on dev (04 §8.3: within 2 code values on flat areas), for 1:1,
// downscaled, preview, letterboxed, cropped and (where the driver rotates) rotated frames. Returns the cases compared.
int CompareVideoProcessorWithShader(Device &dev, const char *label) {
  std::unique_ptr<im::gpu::VideoProcessorConverter> probe;
  const bool rotates =
      im::gpu::VideoProcessorConverter::Create(dev, Size{1080, 1920}, Size{1280, 720}, 60, &probe) == IM_OK &&
      probe->rotates();
  im::abi::ClearLastError();
  std::printf("[ info     ] %s: VideoProcessor rotation %s\n", label, rotates ? "yes" : "no");

  std::vector<ConvertCase> cases = {
      {"1080p 1:1", {1920, 1080}, {1920, 1080}, {}, Rotation::kIdentity},
      {"1440p to 1080p", {2560, 1440}, {2560, 1440}, {2073600, 0, 0}, Rotation::kIdentity},
      {"preview 640x360 of 1080p", {1920, 1080}, {1920, 1080}, {230400, 0, 0}, Rotation::kIdentity},
      {"letterboxed 120x40", {120, 40}, {120, 40}, {}, Rotation::kIdentity},
      {"content inside a larger texture", {1000, 700}, {1280, 800}, {}, Rotation::kIdentity},
  };
  if (rotates) {
    cases.push_back({"panel turned 90, to 720p", {1080, 1920}, {1080, 1920}, {921600, 0, 0}, Rotation::k90});
  }
  uint32_t seed = 11;
  int compared = 0;
  for (const ConvertCase &c : cases) {
    SCOPED_TRACE(c.name);
    const BgraImage tex_img = wt::Embed(wt::TestImage(c.content.width, c.content.height, seed++), c.texture.width,
                                        c.texture.height, kMagenta);
    const Texture tex = Upload(dev, tex_img);
    const Size upright = im::gpu::Upright(c.content, c.rotation);
    const OutputSize layer = im::video::ComputeOutputSize(upright, c.bounds);
    const std::unique_ptr<Converter> vp = MakeConverter(dev, layer, upright, true, c.texture);
    const std::unique_ptr<Converter> shader = MakeConverter(dev, layer, upright, false, c.texture);
    if (!tex.tex || !vp || !shader) {
      ADD_FAILURE() << label << ": setup failed";
      continue;
    }
    if (vp->path() != Converter::Path::kVideoProcessor) {
      std::printf("[ info     ] %s, %s: no VideoProcessor for it, skipped\n", label, c.name);
      continue;
    }
    ++compared;
    const SourceFrame src = tex.Source(c.content, c.rotation);
    const ConvertGeometry g = vp->Geometry(src);
    const Nv12Image got = ConvertAndRead(dev, *vp, src);
    EXPECT_EQ(vp->path(), Converter::Path::kVideoProcessor) << label; // no fallback happened
    const Nv12Image ref = ConvertAndRead(dev, *shader, src);
    if (got.y.empty() || ref.y.empty()) {
      continue; // ConvertAndRead reported it
    }
    // Flat areas only (04 §8.3): drivers filter edges and the content's border their own way, so a pixel counts when
    // the texels around it (3 to each side) have one color and it is at least 3 pixels from the content's edge;
    // inside the bars, at least 3 pixels from it.
    const wt::Nv12Diff d = wt::CompareNv12(got, ref, [&](Point p) {
      const double e = DistanceToDestEdge(g, p);
      if (e < 0) {
        return e <= -3 ? 1 : 0;
      }
      return e >= 3 && model::Flat(tex_img, g, p, 3) ? 1 : 0;
    });
    std::printf("[ info     ] %s, VideoProcessor vs shader, %s: %s\n", label, c.name, d.Summary().c_str());
    EXPECT_LE(d.max_flat, 2) << label << ": " << d.Summary();
    EXPECT_GT(d.flat, static_cast<uint64_t>(layer.size.width) * layer.size.height / 8);
    if (d.max_flat > 2) {
      WriteActualNv12(std::string("vp_") + std::to_string(seed - 1), got);
      WriteActualNv12(std::string("vp_") + std::to_string(seed - 1) + "_shader", ref);
    }
  }
  return compared;
}

TEST_F(GpuWarp, VideoProcessorMatchesShader) {
  std::string why;
  if (!HasVideoProcessor(*dev_, &why)) {
    GTEST_SKIP() << "WARP offers no VideoProcessor for BGRA to BT.709 limited-range NV12: " << why;
  }
  // The 1080p case at least: HasVideoProcessor created a VideoProcessor for it.
  EXPECT_GT(CompareVideoProcessorWithShader(*dev_, "WARP"), 0);
}

// The owner's-PC check of README WA33 ("on each available GPU vendor"): with IM_GPU_TEST_HARDWARE=1 the same comparison
// runs on every hardware adapter DXGI lists. Run on the owner's PC (both OSes):
//   set IM_GPU_TEST_HARDWARE=1 && gpu_warp_test.exe --gtest_filter=GpuHardware.*
TEST(GpuHardware, VideoProcessorMatchesShaderOnEachAdapter) {
  if (Env("IM_GPU_TEST_HARDWARE").empty()) {
    GTEST_SKIP() << "set IM_GPU_TEST_HARDWARE=1 to compare the VideoProcessor with the shader on each hardware adapter";
  }
  ComPtr<IDXGIFactory1> factory;
  ASSERT_TRUE(SUCCEEDED(CreateDXGIFactory1(IID_PPV_ARGS(&factory))));
  int adapters = 0;
  ComPtr<IDXGIAdapter1> adapter;
  for (UINT i = 0; factory->EnumAdapters1(i, adapter.ReleaseAndGetAddressOf()) != DXGI_ERROR_NOT_FOUND; ++i) {
    DXGI_ADAPTER_DESC1 desc{};
    if (FAILED(adapter->GetDesc1(&desc)) || (desc.Flags & DXGI_ADAPTER_FLAG_SOFTWARE) != 0 ||
        (desc.VendorId == im::gpu::kVendorMicrosoft && desc.DeviceId == im::gpu::kBasicRenderDriverDevice)) {
      continue;
    }
    char label[64];
    std::snprintf(label, sizeof label, "adapter %u (vendor 0x%04X device 0x%04X)", i, desc.VendorId, desc.DeviceId);
    SCOPED_TRACE(label);
    DeviceRequest r;
    r.adapter = DeviceRequest::Adapter::kLuid;
    r.luid = (static_cast<uint64_t>(static_cast<uint32_t>(desc.AdapterLuid.HighPart)) << 32) | desc.AdapterLuid.LowPart;
    std::unique_ptr<Device> dev;
    ASSERT_EQ(Device::Create(r, &dev), IM_OK) << Detail();
    ++adapters;
    std::string why;
    if (!HasVideoProcessor(*dev, &why)) {
      ADD_FAILURE() << label << " offers no VideoProcessor: " << why;
      continue;
    }
    EXPECT_GT(CompareVideoProcessorWithShader(*dev, label), 0);
  }
  if (adapters == 0) {
    GTEST_SKIP() << "no hardware adapter";
  }
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
TEST_F(GpuWarp, FailedVideoProcessorBltFallsBackToShader) {
  std::string why;
  if (!VideoProcessorAvailable(&why)) {
    GTEST_SKIP() << "WARP offers no VideoProcessor: " << why;
  }
  const BgraImage img = wt::TestImage(1280, 720, 5);
  const Texture tex = Upload(*dev_, img);
  ASSERT_FALSE(HasFailure());
  const Size source{1280, 720};
  const OutputSize layer = im::video::ComputeOutputSize(source, {});
  const std::unique_ptr<Converter> conv = MakeConverter(*dev_, layer, source, true);
  const std::unique_ptr<Converter> shader = MakeConverter(*dev_, layer, source, false);
  ASSERT_TRUE(conv && shader);
  if (conv->path() != Converter::Path::kVideoProcessor) {
    GTEST_SKIP() << "WARP offers no VideoProcessor for 1280x720";
  }
  conv->FailNextVideoProcessorBltForTest();
  const SourceFrame src = tex.Source(source, Rotation::kIdentity);
  const Nv12Image got = ConvertAndRead(*dev_, *conv, src); // the failed Blt is converted again by the shader
  EXPECT_EQ(conv->path(), Converter::Path::kShader);
  const Nv12Image ref = ConvertAndRead(*dev_, *shader, src);
  ASSERT_FALSE(HasFailure());
  EXPECT_EQ(got.y, ref.y);
  EXPECT_EQ(got.uv, ref.uv);
  // For good: the next frame takes the shader too.
  (void)ConvertAndRead(*dev_, *conv, src);
  EXPECT_EQ(conv->path(), Converter::Path::kShader);
}
#endif

// -------------------------------------------------------------------------------------------------- letterbox

TEST_F(GpuWarp, LetterboxGolden) {
  const struct {
    const char *source;
    const char *golden;
    Rect content;
  } cases[] = {
      {"letterbox_wide_source.txt", "letterbox_wide.txt", Rect{0, 12, 120, 40}},
      {"letterbox_tall_source.txt", "letterbox_tall.txt", Rect{12, 0, 40, 120}},
  };
  for (const auto &c : cases) {
    SCOPED_TRACE(c.golden);
    const BgraImage src = Golden(c.source);
    const BgraImage want = Golden(c.golden);
    ASSERT_FALSE(HasFailure());
    const Size size{src.width, src.height};
    const OutputSize layer = im::video::ComputeOutputSize(size, {});
    EXPECT_TRUE(layer.letterboxed);
    EXPECT_TRUE(layer.size == (Size{want.width, want.height}));
    EXPECT_TRUE(layer.content == c.content);
    const Texture tex = Upload(*dev_, src);
    const std::unique_ptr<Converter> conv = MakeConverter(*dev_, layer, size, false);
    ASSERT_TRUE(conv);
    const SourceFrame frame = tex.Source(size, Rotation::kIdentity);
    const ConvertGeometry g = conv->Geometry(frame);
    const Nv12Image got = ConvertAndRead(*dev_, *conv, frame);
    ASSERT_FALSE(HasFailure());
    const wt::Nv12Diff d = wt::CompareNv12(got, model::Convert(want, Identity(want)), [](Point) { return 1; });
    EXPECT_LE(d.max_flat, 1) << d.Summary();
    // The bars are exactly black: Y 16, Cb/Cr 128.
    uint64_t bar_pixels = 0;
    for (uint32_t y = 0; y < got.height; ++y) {
      for (uint32_t x = 0; x < got.width; ++x) {
        if (!model::InsideDest(g, model::LumaSite(x, y))) {
          ++bar_pixels;
          EXPECT_EQ(got.Y(x, y), model::kBlackY) << "Y(" << x << "," << y << ")";
        }
      }
    }
    for (uint32_t j = 0; j < got.height / 2; ++j) {
      for (uint32_t i = 0; i < got.width / 2; ++i) {
        if (!model::InsideDest(g, model::ChromaSite(i, j))) {
          EXPECT_EQ(got.Cb(i, j), model::kBlackC) << "Cb(" << i << "," << j << ")";
          EXPECT_EQ(got.Cr(i, j), model::kBlackC) << "Cr(" << i << "," << j << ")";
        }
      }
    }
    EXPECT_EQ(bar_pixels, static_cast<uint64_t>(layer.size.width) * layer.size.height -
                              static_cast<uint64_t>(c.content.width) * c.content.height);
    if (d.max_flat > 1) {
      WriteActualNv12(c.golden, got);
    }
  }

  // Clamped and scaled: a 1000x100 window in a layer 400 wide → 400x64, the content 400x40 at y 12, downscaled 2.5:1.
  const Size wide{1000, 100};
  const OutputSize layer = im::video::ComputeOutputSize(wide, LayerBounds{0, 400, 0});
  EXPECT_TRUE(layer.size == (Size{400, 64}));
  EXPECT_TRUE(layer.content == (Rect{0, 12, 400, 40}));
  const BgraImage img = wt::TestImage(wide.width, wide.height, 9);
  const Texture tex = Upload(*dev_, img);
  const std::unique_ptr<Converter> conv = MakeConverter(*dev_, layer, wide, false);
  ASSERT_TRUE(conv);
  const SourceFrame frame = tex.Source(wide, Rotation::kIdentity);
  const ConvertGeometry g = conv->Geometry(frame);
  const Nv12Image got = ConvertAndRead(*dev_, *conv, frame);
  ASSERT_FALSE(HasFailure());
  const wt::Nv12Diff d =
      wt::CompareNv12(got, model::Convert(img, g), [&](Point p) { return model::Flat(img, g, p, 1) ? 1 : 2; });
  EXPECT_LE(d.max_flat, 1) << d.Summary();
  EXPECT_LE(d.max_edge, 2) << d.Summary();
}

// --------------------------------------------------------------------------------------------------- rotation

TEST_F(GpuWarp, RotationGolden) {
  const BgraImage src = Golden("rotation_source.txt");
  ASSERT_FALSE(HasFailure());
  const Size size{src.width, src.height};
  const Texture tex = Upload(*dev_, src);
  std::string why;
  const bool vp_available = VideoProcessorAvailable(&why);
  const struct {
    Rotation r;
    const char *golden;
  } cases[] = {
      {Rotation::k90, "rotation_90.txt"}, {Rotation::k180, "rotation_180.txt"}, {Rotation::k270, "rotation_270.txt"}};
  for (const auto &c : cases) {
    SCOPED_TRACE(c.golden);
    const BgraImage want = Golden(c.golden);
    ASSERT_FALSE(HasFailure());
    const Size upright = im::gpu::Upright(size, c.r);
    ASSERT_TRUE(upright == (Size{want.width, want.height}));
    const OutputSize layer = im::video::ComputeOutputSize(upright, {});
    ASSERT_TRUE(layer.size == upright);
    const Nv12Image expected = model::Convert(want, Identity(want));
    const SourceFrame frame = tex.Source(size, c.r);
    for (const bool use_vp : {false, true}) {
      if (use_vp && !vp_available) {
        std::printf("[ info     ] rotation %s: no VideoProcessor on WARP (%s)\n", RotationName(c.r), why.c_str());
        continue;
      }
      const std::unique_ptr<Converter> conv = MakeConverter(*dev_, layer, upright, use_vp);
      ASSERT_TRUE(conv);
      const Nv12Image got = ConvertAndRead(*dev_, *conv, frame);
      ASSERT_FALSE(HasFailure());
      if (use_vp) {
        // The VideoProcessor rotates only where the driver says so; otherwise the frame took the shader.
        const ConvertGeometry g = Identity(want);
        const wt::Nv12Diff d =
            wt::CompareNv12(got, expected, [&](Point p) { return model::Flat(want, g, p, 3) ? 1 : 0; });
        std::printf("[ info     ] rotation %s through the converter with the VideoProcessor: %s\n", RotationName(c.r),
                    d.Summary().c_str());
        EXPECT_LE(d.max_flat, 2) << d.Summary();
      } else {
        const wt::Nv12Diff d = wt::CompareNv12(got, expected, [](Point) { return 1; });
        EXPECT_LE(d.max_flat, 1) << d.Summary();
        if (d.max_flat > 1) {
          WriteActualNv12(c.golden, got);
        }
      }
    }
  }
}

// ---------------------------------------------------------------------------------------------------- pointer

CursorImage PointerImage(const char *file, uint32_t type) {
  const BgraImage pic = Golden(file);
  const wt::ShapeBuffer b = type == im::gpu::kPointerMonochrome ? wt::MonochromeShape(pic) : wt::ColorShape(pic, type);
  CursorImage img;
  EXPECT_TRUE(im::gpu::ConvertPointerShape(b.shape, b.bytes, &img)) << file;
  return img;
}

TEST(GpuPointer, ShapesConvert) {
  const CursorImage color = PointerImage("pointer_color.txt", im::gpu::kPointerColor);
  EXPECT_EQ(wt::FromCursorImage(color).bgra, Golden("pointer_color.txt").bgra); // as they are
  EXPECT_EQ(wt::FromCursorImage(PointerImage("pointer_masked.txt", im::gpu::kPointerMaskedColor)).bgra,
            Golden("pointer_masked_converted.txt").bgra);
  EXPECT_EQ(wt::FromCursorImage(PointerImage("pointer_mono.txt", im::gpu::kPointerMonochrome)).bgra,
            Golden("pointer_mono_converted.txt").bgra);

  // Bad shapes: an unknown type, a zero size, an odd monochrome height, a short pitch or buffer.
  CursorImage out;
  std::vector<uint8_t> buf(4096, 0);
  EXPECT_FALSE(im::gpu::ConvertPointerShape({3, 16, 16, 64}, buf, &out));
  EXPECT_FALSE(im::gpu::ConvertPointerShape({im::gpu::kPointerColor, 0, 16, 64}, buf, &out));
  EXPECT_FALSE(im::gpu::ConvertPointerShape({im::gpu::kPointerMonochrome, 16, 31, 4}, buf, &out));
  EXPECT_FALSE(im::gpu::ConvertPointerShape({im::gpu::kPointerColor, 16, 16, 60}, buf, &out));
  EXPECT_FALSE(im::gpu::ConvertPointerShape({im::gpu::kPointerColor, 16, 128, 64}, buf, &out));
  EXPECT_TRUE(out.pixels.empty());
}

struct PointerScene {
  const char *golden;
  const char *shape;
  uint32_t type;
  int32_t x;
  int32_t y;
  Rotation rotation;
  Size frame; // the slot's content, in the capture's orientation
};

const PointerScene kScenes[] = {
    {"cursor_color.txt", "pointer_color.txt", im::gpu::kPointerColor, 4, 6, Rotation::kIdentity, {64, 64}},
    {"cursor_masked.txt", "pointer_masked.txt", im::gpu::kPointerMaskedColor, 38, 5, Rotation::kIdentity, {64, 64}},
    {"cursor_mono.txt", "pointer_mono.txt", im::gpu::kPointerMonochrome, 6, 37, Rotation::kIdentity, {64, 64}},
    {"cursor_clipped.txt", "pointer_mono.txt", im::gpu::kPointerMonochrome, 52, 54, Rotation::kIdentity, {64, 64}},
    {"cursor_rot90.txt", "pointer_color.txt", im::gpu::kPointerColor, 10, 20, Rotation::k90, {64, 48}},
    {"cursor_rot270.txt", "pointer_color.txt", im::gpu::kPointerColor, 30, 40, Rotation::k270, {64, 48}},
};

// Publishes a frame of background with the pointer at (x, y) and takes it (the video thread's side).
Slot *PublishPointerFrame(Device &d, FrameSlots &slots, const BgraImage &background, const PointerScene &s,
                          bool visible) {
  Slot *w = nullptr;
  if (slots.BeginWrite(s.frame, &w) != IM_OK) {
    ADD_FAILURE() << "BeginWrite: " << Detail();
    return nullptr;
  }
  FillSlot(d, *w, background);
  FrameMeta meta;
  meta.capture_ts_ns = 1;
  meta.content = s.frame;
  meta.rotation = s.rotation;
  meta.cursor.visible = visible;
  meta.cursor.x = s.x;
  meta.cursor.y = s.y;
  slots.Publish(meta);
  const FrameSlots::Taken t = slots.TakeLatest();
  EXPECT_TRUE(t.fresh);
  return t.slot;
}

TEST_F(GpuWarp, PointerBlendGolden) {
  for (const PointerScene &s : kScenes) {
    SCOPED_TRACE(s.golden);
    std::unique_ptr<FrameSlots> slots;
    ASSERT_EQ(FrameSlots::Create(*dev_, s.frame, &slots), IM_OK) << Detail();
    std::unique_ptr<CursorOverlay> overlay;
    ASSERT_EQ(CursorOverlay::Create(*dev_, 0, &overlay), IM_OK) << Detail();
    EXPECT_FALSE(overlay->hidden());
    overlay->SetShape(PointerImage(s.shape, s.type));
    const BgraImage background = wt::Quadrants(s.frame.width, s.frame.height);
    Slot *slot = PublishPointerFrame(*dev_, *slots, background, s, true);
    ASSERT_NE(slot, nullptr);
    ASSERT_EQ(overlay->Blend(*slot), IM_OK) << Detail();
    EXPECT_TRUE(slot->cursor_blended);
    const BgraImage got = ReadBgra(*dev_, slot->texture.Get());
    const BgraImage want = Golden(s.golden);
    ASSERT_FALSE(HasFailure());
    std::string where;
    const int diff = wt::MaxRgbDiff(got, want, &where);
    EXPECT_LE(diff, 1) << where;
    if (diff > 1) {
      WriteActual(s.golden, got);
    }
    // Once per frame: a second Blend of the same frame draws nothing more.
    ASSERT_EQ(overlay->Blend(*slot), IM_OK);
    EXPECT_EQ(ReadBgra(*dev_, slot->texture.Get()).bgra, got.bgra);
  }
}

TEST_F(GpuWarp, HiddenOrInvisiblePointerDrawsNothing) {
  const PointerScene &s = kScenes[0];
  const BgraImage background = wt::Quadrants(s.frame.width, s.frame.height);
  for (const bool hide : {true, false}) {
    SCOPED_TRACE(hide ? "IM_START_HIDE_CURSOR" : "pointer not visible");
    std::unique_ptr<FrameSlots> slots;
    ASSERT_EQ(FrameSlots::Create(*dev_, s.frame, &slots), IM_OK) << Detail();
    std::unique_ptr<CursorOverlay> overlay;
    ASSERT_EQ(CursorOverlay::Create(*dev_, hide ? IM_START_HIDE_CURSOR : 0u, &overlay), IM_OK) << Detail();
    EXPECT_EQ(overlay->hidden(), hide);
    overlay->SetShape(PointerImage(s.shape, s.type));
    Slot *slot = PublishPointerFrame(*dev_, *slots, background, s, /*visible=*/hide);
    ASSERT_NE(slot, nullptr);
    ASSERT_EQ(overlay->Blend(*slot), IM_OK);
    EXPECT_FALSE(slot->cursor_blended);
    EXPECT_EQ(ReadBgra(*dev_, slot->texture.Get()).bgra, background.bgra);
  }
}

// ----------------------------------------------------------------------------------------------- test pattern

TEST_F(GpuWarp, TestPatternRenders) {
  std::unique_ptr<FrameSlots> slots;
  ASSERT_EQ(FrameSlots::Create(*dev_, Size{1920, 1080}, &slots), IM_OK) << Detail();
  std::unique_ptr<TestPattern> pattern;
  ASSERT_EQ(TestPattern::Create(*dev_, &pattern), IM_OK) << Detail();
  const struct {
    Size size;
    im::gpu::PatternFrame frame;
  } frames[] = {
      {{1920, 1080}, {0, 0}},                // a flash
      {{1920, 1080}, {37, 616'666'666}},     // no flash, the bar mid-way
      {{1280, 720}, {65538, 1'050'000'000}}, // a smaller frame in the slot, a flash, the counter wrapped
      {{1920, 1080}, {119, 1'983'333'333}},  // the bar near the right end
      {{64, 64}, {0xFFFF, 3'000'000'000}},   // the smallest output size, every counter bit set
  };
  std::vector<BgraImage> rendered;
  for (const auto &f : frames) {
    SCOPED_TRACE(std::to_string(f.frame.index));
    Slot *w = nullptr;
    ASSERT_EQ(slots->BeginWrite(f.size, &w), IM_OK) << Detail();
    ASSERT_EQ(pattern->Render(*w, f.size, f.frame), IM_OK) << Detail();
    FrameMeta meta;
    meta.capture_ts_ns = 1000 + f.frame.elapsed_ns;
    meta.content = f.size;
    const uint64_t seq = slots->Publish(meta);
    const FrameSlots::Taken t = slots->TakeLatest();
    ASSERT_TRUE(t.fresh);
    ASSERT_EQ(t.slot->seq, seq);
    EXPECT_TRUE(t.slot->meta.content == f.size);
    const BgraImage all = ReadBgra(*dev_, t.slot->texture.Get());
    ASSERT_FALSE(HasFailure());
    const im::gpu::PatternLayout l = im::gpu::MakePatternLayout(f.size.width, f.size.height);
    BgraImage got(f.size.width, f.size.height);
    uint64_t wrong = 0;
    std::string first;
    for (uint32_t y = 0; y < f.size.height; ++y) {
      for (uint32_t x = 0; x < f.size.width; ++x) {
        const uint32_t g = all.Get(x, y);
        got.Set(x, y, g);
        const uint32_t want = im::gpu::PatternPixel(l, f.frame, x, y);
        if (g != want) {
          if (wrong++ == 0) {
            char buf[96];
            std::snprintf(buf, sizeof buf, "(%u,%u) got %08X want %08X", x, y, g, want);
            first = buf;
          }
        }
      }
    }
    EXPECT_EQ(wrong, 0u) << "first: " << first;
    if (wrong != 0) {
      WriteActual("pattern_" + std::to_string(f.frame.index) + ".txt", got);
    }
    rendered.push_back(got);
  }
  // The pattern moves: frames 0 and 37 differ in the moving bar's band and the counter, not in the static half.
  const BgraImage &a = rendered[0];
  const BgraImage &b = rendered[1];
  const im::gpu::PatternLayout l = im::gpu::MakePatternLayout(1920, 1080);
  bool static_same = true;
  bool moved = false;
  for (uint32_t y = 0; y < 1080; ++y) {
    for (uint32_t x = 0; x < 1920; ++x) {
      if (y < l.ramp_end) {
        static_same = static_same && a.Get(x, y) == b.Get(x, y);
      } else if (y < l.mover_end) {
        moved = moved || a.Get(x, y) != b.Get(x, y);
      }
    }
  }
  EXPECT_TRUE(static_same);
  EXPECT_TRUE(moved);

  // Through the converter: the pattern's NV12 against the CPU model.
  const Size size{1920, 1080};
  Slot *w = nullptr;
  ASSERT_EQ(slots->BeginWrite(size, &w), IM_OK);
  ASSERT_EQ(pattern->Render(*w, size, {5, 83'333'333}), IM_OK);
  FrameMeta meta;
  meta.content = size;
  slots->Publish(meta);
  Slot *slot = slots->TakeLatest().slot;
  ASSERT_NE(slot, nullptr);
  const BgraImage img = ReadBgra(*dev_, slot->texture.Get());
  const OutputSize layer = im::video::ComputeOutputSize(size, LayerBounds{921600, 0, 0});
  const std::unique_ptr<Converter> conv = MakeConverter(*dev_, layer, size, false);
  ASSERT_TRUE(conv);
  const SourceFrame src = slot->source();
  const ConvertGeometry g = conv->Geometry(src);
  const Nv12Image got = ConvertAndRead(*dev_, *conv, src);
  ASSERT_FALSE(HasFailure());
  const wt::Nv12Diff d =
      wt::CompareNv12(got, model::Convert(img, g), [&](Point p) { return model::Flat(img, g, p, 1) ? 1 : 2; });
  EXPECT_LE(d.max_flat, 1) << d.Summary();
  EXPECT_LE(d.max_edge, 2) << d.Summary();
}

// The session's flow on two threads (gpu.h): a capture side drawing pattern frames into the slots on a 60 Hz grid,
// and a video thread taking the newest frame and converting it for the full layer (every frame) and the preview (every
// fourth), both on one immediate context under the context lock. Each converted frame's counter, read back from the
// NV12 luma, is the frame its capture time says.
TEST_F(GpuWarp, PatternThroughSlotsOnTwoThreads) {
  const Size size{1280, 720};
  std::unique_ptr<FrameSlots> slots;
  ASSERT_EQ(FrameSlots::Create(*dev_, size, &slots), IM_OK) << Detail();
  std::unique_ptr<TestPattern> pattern;
  ASSERT_EQ(TestPattern::Create(*dev_, &pattern), IM_OK) << Detail();
  const std::unique_ptr<Converter> full = MakeConverter(*dev_, im::video::ComputeOutputSize(size, {}), size, true);
  const std::unique_ptr<Converter> preview =
      MakeConverter(*dev_, im::video::ComputeOutputSize(size, LayerBounds{230400, 0, 0}), size, true);
  ASSERT_TRUE(full && preview);
  std::unique_ptr<Nv12Surface> full_out;
  std::unique_ptr<Nv12Surface> preview_out;
  ASSERT_EQ(Nv12Surface::Create(*dev_, full->config().output.size, &full_out), IM_OK) << Detail();
  ASSERT_EQ(Nv12Surface::Create(*dev_, preview->config().output.size, &preview_out), IM_OK) << Detail();
  std::unique_ptr<Nv12Readback> readback;
  ASSERT_EQ(Nv12Readback::Create(*dev_, size, full_out->planar(), &readback), IM_OK) << Detail();

  constexpr int64_t kInterval = 16'666'667; // ns
  constexpr uint64_t kFrames = 60;
  std::atomic<bool> done{false};
  std::atomic<int> writer_errors{0};
  std::thread capture([&] {
    const auto start = std::chrono::steady_clock::now();
    for (uint64_t k = 0; k < kFrames; ++k) {
      std::this_thread::sleep_until(start + std::chrono::nanoseconds(static_cast<int64_t>(k) * kInterval));
      Slot *w = nullptr;
      const int64_t elapsed = static_cast<int64_t>(k) * kInterval;
      if (slots->BeginWrite(size, &w) != IM_OK || pattern->Render(*w, size, {k, elapsed}) != IM_OK) {
        writer_errors.fetch_add(1);
        break;
      }
      FrameMeta meta;
      meta.capture_ts_ns = elapsed; // the capture time since epoch_ns: frame k on the grid
      meta.content = size;
      slots->Publish(meta);
    }
    done.store(true);
  });

  const im::gpu::PatternLayout l = im::gpu::MakePatternLayout(size.width, size.height);
  const uint32_t counter_row = (l.mover_end + l.counter_end) / 2;
  Nv12Image img(size.width, size.height);
  const im::gpu::Nv12Planes planes{img.y.data(), size.width, img.uv.data(), size.width};
  uint64_t taken = 0;
  uint64_t skipped = 0;
  uint64_t last_seq = 0;
  int wrong = 0;
  std::string first_wrong;
  const auto take = [&] {
    const FrameSlots::Taken t = slots->TakeLatest();
    if (!t.fresh) {
      return;
    }
    ++taken;
    skipped += t.skipped;
    EXPECT_GT(t.slot->seq, last_seq);
    last_seq = t.slot->seq;
    const SourceFrame src = t.slot->source();
    ASSERT_EQ(full->Convert(src, full_out->target()), IM_OK) << Detail();
    if (taken % 4 == 0) {
      ASSERT_EQ(preview->Convert(src, preview_out->target()), IM_OK) << Detail();
    }
    ASSERT_EQ(readback->Read(full_out->target(), planes), IM_OK) << Detail();
    // The 16 counter blocks: white (Y 235) where the frame index's bit is set, black (Y 16) elsewhere.
    uint32_t counter = 0;
    for (uint32_t i = 0; i < 16; ++i) {
      const uint32_t x = (2 * i + 1) * size.width / 32; // the block's middle
      counter = (counter << 1) | (img.Y(x, counter_row) > 128 ? 1u : 0u);
    }
    const auto k = static_cast<uint32_t>((t.slot->meta.capture_ts_ns + kInterval / 2) / kInterval);
    if (counter != (k & 0xFFFF) && wrong++ == 0) {
      first_wrong = "frame " + std::to_string(k) + " shows counter " + std::to_string(counter);
    }
  };
  while (!done.load()) {
    take();
    if (HasFatalFailure()) {
      break;
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(4));
  }
  capture.join();
  ASSERT_FALSE(HasFatalFailure());
  take(); // the last frame
  EXPECT_EQ(writer_errors.load(), 0);
  EXPECT_EQ(wrong, 0) << first_wrong;
  EXPECT_EQ(taken + skipped, kFrames); // every published frame was taken or skipped (newest wins)
  EXPECT_GE(taken, kFrames / 4);
  std::printf("[ info     ] two threads: %llu frames taken, %llu skipped, full layer path %s\n",
              static_cast<unsigned long long>(taken), static_cast<unsigned long long>(skipped),
              full->path() == Converter::Path::kVideoProcessor ? "VideoProcessor" : "shader");
}

// ------------------------------------------------------------------------------------------------ frame slots

void Clear(Device &d, Slot &slot, float r, float g, float b) {
  const float color[4] = {r, g, b, 1.0f};
  ContextLock lock(d);
  d.context()->ClearRenderTargetView(slot.rtv.Get(), color);
}

uint32_t TopLeft(Device &d, Slot &slot) { return ReadBgra(d, slot.texture.Get()).Get(0, 0); }

TEST_F(GpuWarp, FrameSlots) {
  std::unique_ptr<FrameSlots> slots;
  ASSERT_EQ(FrameSlots::Create(*dev_, Size{64, 64}, &slots), IM_OK) << Detail();
  EXPECT_EQ(slots->TakeLatest().slot, nullptr); // nothing published yet
  EXPECT_EQ(slots->current(), nullptr);

  const auto publish = [&](float r, float g, float b, int64_t ts) {
    Slot *w = nullptr;
    EXPECT_EQ(slots->BeginWrite(Size{64, 64}, &w), IM_OK);
    Clear(*dev_, *w, r, g, b);
    FrameMeta meta;
    meta.capture_ts_ns = ts;
    meta.content = Size{64, 64};
    slots->Publish(meta);
    return w;
  };
  publish(1, 0, 0, 1);
  FrameSlots::Taken t = slots->TakeLatest();
  ASSERT_TRUE(t.fresh);
  EXPECT_EQ(t.skipped, 0u);
  EXPECT_EQ(t.slot->meta.capture_ts_ns, 1);
  EXPECT_EQ(TopLeft(*dev_, *t.slot), 0xFFFF0000u);

  // Two frames before the reader looks: the newest wins, one skipped.
  publish(0, 1, 0, 2);
  publish(0, 0, 1, 3);
  t = slots->TakeLatest();
  ASSERT_TRUE(t.fresh);
  EXPECT_EQ(t.skipped, 1u);
  EXPECT_EQ(t.slot->meta.capture_ts_ns, 3);
  EXPECT_EQ(TopLeft(*dev_, *t.slot), 0xFF0000FFu);
  Slot *held = t.slot;

  // Nothing new: the reader keeps its frame (the last frame, for refreshes and static keyframes).
  t = slots->TakeLatest();
  EXPECT_FALSE(t.fresh);
  EXPECT_EQ(t.slot, held);
  EXPECT_EQ(slots->current(), held);

  // The writer never writes the reader's slot.
  for (int i = 0; i < 6; ++i) {
    Slot *w = publish(1, 1, 1, 10 + i);
    EXPECT_NE(w, held);
  }
  EXPECT_EQ(TopLeft(*dev_, *held), 0xFF0000FFu);
  t = slots->TakeLatest();
  EXPECT_TRUE(t.fresh);
  EXPECT_EQ(t.skipped, 5u);
  EXPECT_EQ(t.slot->meta.capture_ts_ns, 15);

  // A larger content grows the writer's texture (a new texture id); the frame says how much of it it fills.
  Slot *w = nullptr;
  ASSERT_EQ(slots->BeginWrite(Size{64, 64}, &w), IM_OK);
  const uint64_t old_id = w->texture_id;
  ASSERT_EQ(slots->BeginWrite(Size{100, 80}, &w), IM_OK); // the same slot: nothing was published in between
  EXPECT_NE(w->texture_id, old_id);
  // A grown side is rounded up to a multiple of 256: a window dragged larger reallocates every 256 pixels of growth,
  // not on every frame.
  EXPECT_TRUE(w->size == (Size{256, 256})) << w->size.width << "x" << w->size.height;
  const uint64_t grown_id = w->texture_id;
  ASSERT_EQ(slots->BeginWrite(Size{250, 200}, &w), IM_OK);
  EXPECT_EQ(w->texture_id, grown_id); // it still fits: the same texture
  ASSERT_EQ(slots->BeginWrite(Size{300, 100}, &w), IM_OK);
  EXPECT_NE(w->texture_id, grown_id);
  EXPECT_TRUE(w->size == (Size{512, 256})) << w->size.width << "x" << w->size.height; // only the side that grew
  FrameMeta meta;
  meta.content = Size{300, 100};
  slots->Publish(meta);
  t = slots->TakeLatest();
  ASSERT_TRUE(t.fresh);
  EXPECT_TRUE(t.slot->meta.content == (Size{300, 100}));
  EXPECT_TRUE(t.slot->source().texture_size == t.slot->size);
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
// The slots' textures differ in size once one grew (only the writer's slot grows): the converter keeps a
// VideoProcessor per slot size (converter.h), so alternating sizes create none after the first frame of each.
TEST_F(GpuWarp, ConverterKeepsAVideoProcessorPerSlotSize) {
  std::string why;
  if (!VideoProcessorAvailable(&why)) {
    GTEST_SKIP() << "WARP offers no VideoProcessor: " << why;
  }
  const Size start{640, 360};
  std::unique_ptr<FrameSlots> slots;
  ASSERT_EQ(FrameSlots::Create(*dev_, start, &slots), IM_OK) << Detail();
  const OutputSize layer = im::video::ComputeOutputSize(start, {});
  const std::unique_ptr<Converter> conv = MakeConverter(*dev_, layer, start, true, start);
  ASSERT_TRUE(conv);
  if (conv->path() != Converter::Path::kVideoProcessor) {
    GTEST_SKIP() << "WARP offers no VideoProcessor for 640x360";
  }
  EXPECT_EQ(conv->video_processors_created_for_test(), 1u); // Create made the one for the slots' size
  std::unique_ptr<Nv12Surface> out;
  ASSERT_EQ(Nv12Surface::Create(*dev_, layer.size, &out), IM_OK) << Detail();
  std::unique_ptr<Nv12Readback> readback;
  ASSERT_EQ(Nv12Readback::Create(*dev_, layer.size, out->planar(), &readback), IM_OK) << Detail();
  Nv12Image img(layer.size.width, layer.size.height);
  const im::gpu::Nv12Planes planes{img.y.data(), layer.size.width, img.uv.data(), layer.size.width};

  // The reader takes every frame, so the writer fills the three slots in turn; each gets a content larger than it
  // holds the first time round, which leaves three textures of three sizes.
  const Size contents[3] = {{700, 400}, {1000, 600}, {1300, 800}};
  const Size grown[3] = {{768, 512}, {1024, 768}, {1536, 1024}};
  const uint8_t colors[3][3] = {{204, 51, 26}, {26, 179, 77}, {51, 77, 230}}; // R, G, B
  uint32_t created_after_first_round = 0;
  for (int i = 0; i < 15; ++i) { // three slots, converted five times each
    SCOPED_TRACE(i);
    const int k = i % 3;
    Slot *w = nullptr;
    ASSERT_EQ(slots->BeginWrite(contents[k], &w), IM_OK) << Detail();
    EXPECT_TRUE(w->size == grown[k]) << w->size.width << "x" << w->size.height;
    Clear(*dev_, *w, colors[k][0] / 255.0f, colors[k][1] / 255.0f, colors[k][2] / 255.0f);
    FrameMeta meta;
    meta.capture_ts_ns = i + 1;
    meta.content = contents[k];
    slots->Publish(meta);
    const FrameSlots::Taken t = slots->TakeLatest();
    ASSERT_TRUE(t.fresh);
    ASSERT_EQ(conv->Convert(t.slot->source(), out->target()), IM_OK) << Detail();
    if (i == 2) {
      created_after_first_round = conv->video_processors_created_for_test();
    }
    // The frame's own color in the middle of the picture (a flat area): the VideoProcessor read the right slot. Within
    // 3 of the model: the VideoProcessor's 2 from the shader plus the shader's 1 from the model.
    ASSERT_EQ(readback->Read(out->target(), planes), IM_OK) << Detail();
    const im::gpu::model::Ycc want =
        im::gpu::model::ToYcc(im::gpu::model::Rgb{colors[k][0] / 255.0, colors[k][1] / 255.0, colors[k][2] / 255.0});
    const uint32_t cx = layer.size.width / 2;
    const uint32_t cy = layer.size.height / 2;
    EXPECT_LE(std::abs(img.Y(cx, cy) - model::Code(want.y)), 3);
    EXPECT_LE(std::abs(img.Cb(cx / 2, cy / 2) - model::Code(want.cb)), 3);
    EXPECT_LE(std::abs(img.Cr(cx / 2, cy / 2) - model::Code(want.cr)), 3);
  }
  EXPECT_EQ(conv->path(), Converter::Path::kVideoProcessor);
  // One per grown slot size in the first round, none after it (one per frame before the cache).
  EXPECT_LE(created_after_first_round - 1, 3u);
  EXPECT_EQ(conv->video_processors_created_for_test(), created_after_first_round);
}

// ConverterConfig::input: Create makes the VideoProcessor for the slots' texture size, so a rotated Desktop
// Duplication session (slots in the panel's orientation, sides swapped against the upright source) creates none on
// its first frame; Reconfigure makes the one for a new output size on its own thread too.
TEST_F(GpuWarp, ConverterMakesItsVideoProcessorForTheSlotSize) {
  std::string why;
  if (!VideoProcessorAvailable(&why)) {
    GTEST_SKIP() << "WARP offers no VideoProcessor: " << why;
  }
  const Size slot{1280, 720};    // a panel turned 90 degrees, as Desktop Duplication delivers it
  const Size upright{720, 1280}; // what viewers see
  std::unique_ptr<Converter> conv =
      MakeConverter(*dev_, im::video::ComputeOutputSize(upright, {}), upright, true, slot);
  ASSERT_TRUE(conv);
  if (conv->path() != Converter::Path::kVideoProcessor) {
    GTEST_SKIP() << "WARP offers no VideoProcessor for 1280x720 to 720x1280";
  }
  EXPECT_EQ(conv->video_processors_created_for_test(), 1u);
  const BgraImage img = wt::TestImage(slot.width, slot.height, 23);
  const Texture tex = Upload(*dev_, img);
  ASSERT_FALSE(HasFailure());
  const SourceFrame src = tex.Source(slot, Rotation::k90);
  (void)ConvertAndRead(*dev_, *conv, src);
  ASSERT_FALSE(HasFailure());
  EXPECT_EQ(conv->video_processors_created_for_test(), 1u); // the first frame made none
  ConverterConfig cfg = conv->config();
  cfg.output = im::video::ComputeOutputSize(upright, LayerBounds{230400, 0, 0});
  ASSERT_EQ(conv->Reconfigure(cfg), IM_OK) << Detail();
  ASSERT_EQ(conv->path(), Converter::Path::kVideoProcessor);
  EXPECT_EQ(conv->video_processors_created_for_test(), 2u);
  (void)ConvertAndRead(*dev_, *conv, src);
  ASSERT_FALSE(HasFailure());
  EXPECT_EQ(conv->video_processors_created_for_test(), 2u);
}
#endif

// ---------------------------------------------------------------------------------------------- NV12 readback

// A plane texture (default usage) holding data, for the readback's planar sources.
ComPtr<ID3D11Texture2D> PlaneTexture(Device &d, uint32_t w, uint32_t h, DXGI_FORMAT format,
                                     const std::vector<uint8_t> &data, uint32_t bytes_per_pixel) {
  D3D11_TEXTURE2D_DESC td{};
  td.Width = w;
  td.Height = h;
  td.MipLevels = 1;
  td.ArraySize = 1;
  td.Format = format;
  td.SampleDesc.Count = 1;
  td.Usage = D3D11_USAGE_DEFAULT;
  td.BindFlags = D3D11_BIND_SHADER_RESOURCE;
  D3D11_SUBRESOURCE_DATA init{};
  init.pSysMem = data.data();
  init.SysMemPitch = w * bytes_per_pixel;
  ComPtr<ID3D11Texture2D> t;
  if (FAILED(d.d3d()->CreateTexture2D(&td, &init, t.GetAddressOf()))) {
    ADD_FAILURE() << "plane texture failed";
  }
  return t;
}

std::vector<uint8_t> Bytes(std::size_t n, uint32_t seed) {
  std::vector<uint8_t> v(n);
  for (std::size_t i = 0; i < n; ++i) {
    v[i] = static_cast<uint8_t>((i * 7 + seed * 31 + (i >> 5)) & 0xFF);
  }
  return v;
}

// The planar readback (our own surfaces on a device without NV12 render targets): the planes come back as written,
// and planes of another size or format are refused (CopyResource would copy nothing and leave the previous frame).
TEST_F(GpuWarp, ReadbackOfPlanes) {
  const Size size{64, 32};
  const std::vector<uint8_t> y = Bytes(64 * 32, 1);
  const std::vector<uint8_t> uv = Bytes(32 * 16 * 2, 2);
  const ComPtr<ID3D11Texture2D> luma = PlaneTexture(*dev_, 64, 32, DXGI_FORMAT_R8_UNORM, y, 1);
  const ComPtr<ID3D11Texture2D> chroma = PlaneTexture(*dev_, 32, 16, DXGI_FORMAT_R8G8_UNORM, uv, 2);
  ASSERT_FALSE(HasFailure());
  std::unique_ptr<Nv12Readback> readback;
  ASSERT_EQ(Nv12Readback::Create(*dev_, size, /*planar=*/true, &readback), IM_OK) << Detail();
  Nv12Image out(size.width, size.height);
  const im::gpu::Nv12Planes planes{out.y.data(), size.width, out.uv.data(), size.width};
  ASSERT_EQ(readback->Read(im::gpu::Nv12Target{nullptr, 0, luma.Get(), chroma.Get()}, planes), IM_OK) << Detail();
  EXPECT_EQ(out.y, y);
  EXPECT_EQ(out.uv, uv);

  const std::vector<uint8_t> other = Bytes(64 * 32 * 2, 3);
  const struct {
    const char *name;
    ComPtr<ID3D11Texture2D> y_plane;
    ComPtr<ID3D11Texture2D> uv_plane;
  } bad[] = {
      {"short luma", PlaneTexture(*dev_, 64, 30, DXGI_FORMAT_R8_UNORM, other, 1), chroma},
      {"luma as R8G8", PlaneTexture(*dev_, 64, 32, DXGI_FORMAT_R8G8_UNORM, other, 2), chroma},
      {"full-size chroma", luma, PlaneTexture(*dev_, 64, 32, DXGI_FORMAT_R8G8_UNORM, other, 2)},
      {"chroma as R8", luma, PlaneTexture(*dev_, 32, 16, DXGI_FORMAT_R8_UNORM, other, 1)},
  };
  ASSERT_FALSE(HasFailure());
  for (const auto &b : bad) {
    SCOPED_TRACE(b.name);
    im::abi::ClearLastError();
    EXPECT_EQ(readback->Read(im::gpu::Nv12Target{nullptr, 0, b.y_plane.Get(), b.uv_plane.Get()}, planes),
              IM_ERR_INVALID_ARG);
    EXPECT_NE(Detail().find("planar source"), std::string::npos) << Detail();
  }
  im::abi::ClearLastError();
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
// 04 §6.1, §8.1: the readback waits for the GPU without the context lock, so the capture copy and FFmpeg's
// submissions go on meanwhile. While it waits, another thread takes the lock.
TEST_F(GpuWarp, ReadbackWaitsWithoutTheLock) {
  const Size size{1280, 720};
  const BgraImage img = wt::TestImage(size.width, size.height, 31);
  const Texture tex = Upload(*dev_, img);
  ASSERT_FALSE(HasFailure());
  const std::unique_ptr<Converter> conv = MakeConverter(*dev_, im::video::ComputeOutputSize(size, {}), size, false);
  ASSERT_TRUE(conv);
  std::unique_ptr<Nv12Surface> surface;
  ASSERT_EQ(Nv12Surface::Create(*dev_, size, &surface), IM_OK) << Detail();
  std::unique_ptr<Nv12Readback> readback;
  ASSERT_EQ(Nv12Readback::Create(*dev_, size, surface->planar(), &readback), IM_OK) << Detail();

  struct Probe {
    Device *dev = nullptr;
    int calls = 0;
    bool lock_free = false;
    std::promise<void> locked;
    std::thread other;
  } probe;
  probe.dev = dev_.get();
  readback->SetWaitHookForTest(
      [](void *ctx) {
        auto *p = static_cast<Probe *>(ctx);
        if (p->calls++ != 0) {
          return;
        }
        std::future<void> f = p->locked.get_future();
        p->other = std::thread([p] {
          p->dev->Lock(); // blocks until Read releases the lock, if it holds it
          p->dev->Unlock();
          p->locked.set_value();
        });
        p->lock_free = f.wait_for(std::chrono::seconds(5)) == std::future_status::ready;
      },
      &probe);
  const SourceFrame src = tex.Source(size, Rotation::kIdentity);
  ASSERT_EQ(conv->Convert(src, surface->target()), IM_OK) << Detail(); // queued GPU work the readback waits for
  Nv12Image got(size.width, size.height);
  const im::gpu::Nv12Planes planes{got.y.data(), size.width, got.uv.data(), size.width};
  const im_status st = readback->Read(surface->target(), planes);
  if (probe.other.joinable()) {
    probe.other.join();
  }
  readback->SetWaitHookForTest(nullptr, nullptr);
  ASSERT_EQ(st, IM_OK) << Detail();
  EXPECT_EQ(probe.calls, 1);
  EXPECT_TRUE(probe.lock_free) << "another thread could not take the context lock while the readback waited";
  // And the picture is the converted frame.
  const ConvertGeometry g = conv->Geometry(src);
  const wt::Nv12Diff d =
      wt::CompareNv12(got, model::Convert(img, g), [&](Point p) { return model::Flat(img, g, p, 1) ? 1 : 2; });
  EXPECT_LE(d.max_flat, 1) << d.Summary();
  EXPECT_LE(d.max_edge, 2) << d.Summary();
}
#endif

TEST(GpuTripleIndex, OneWriterOneReader) {
  im::gpu::TripleIndex s;
  std::vector<uint64_t> data(im::gpu::TripleIndex::kSlots, 0);
  std::atomic<bool> done{false};
  constexpr uint64_t kFrames = 200'000;
  std::thread writer([&] {
    for (uint64_t i = 1; i <= kFrames; ++i) {
      data[s.write_index()] = i;
      s.Publish();
    }
    done.store(true);
  });
  uint64_t last = 0;
  uint64_t bad = 0;
  while (!done.load()) {
    const im::gpu::TripleIndex::Taken t = s.Take();
    if (t.fresh) {
      if (data[t.index] != t.seq || t.seq <= last) {
        ++bad;
      }
      last = t.seq;
    }
  }
  writer.join();
  const im::gpu::TripleIndex::Taken t = s.Take();
  EXPECT_EQ(bad, 0u);
  EXPECT_EQ(t.seq, kFrames);
  EXPECT_EQ(data[t.index], kFrames);
}

// ----------------------------------------------------------------------------------------------------- device

TEST_F(GpuWarp, ContextLockIsRecursive) {
  dev_->Lock();
  dev_->Lock(); // FFmpeg takes it again while holding it (04 §8.1)
  Device::LockCallback(dev_->lock_ctx());
  std::atomic<bool> entered{false};
  std::thread other([&] {
    dev_->Lock();
    entered.store(true);
    dev_->Unlock();
  });
  std::this_thread::sleep_for(std::chrono::milliseconds(100));
  EXPECT_FALSE(entered.load()); // held by this thread
  Device::UnlockCallback(dev_->lock_ctx());
  dev_->Unlock();
  dev_->Unlock();
  other.join();
  EXPECT_TRUE(entered.load());
}

TEST_F(GpuWarp, DeviceLossMapping) {
  EXPECT_FALSE(dev_->Removed());
  im::abi::ClearLastError();
  EXPECT_EQ(dev_->Fail(E_OUTOFMEMORY, "test"), IM_ERR_INTERNAL);
  EXPECT_EQ(Detail().rfind("gpu: test failed", 0), 0u) << Detail();
  EXPECT_EQ(dev_->Fail(DXGI_ERROR_DEVICE_REMOVED, "test"), IM_ERR_ENCODER_LOST);
  EXPECT_EQ(Detail().rfind("device_removed: ", 0), 0u) << Detail();
  EXPECT_EQ(dev_->Fail(DXGI_ERROR_DEVICE_RESET, "test"), IM_ERR_ENCODER_LOST);
  im::abi::ClearLastError();
}

TEST(GpuDevice, Adapters) {
  // The default adapter (on a runner without a GPU: the Basic Render Driver, which is WARP too) and the same one by
  // its LUID, as a session opens the chosen encoder's adapter (04 §8.1).
  std::unique_ptr<Device> def;
  ASSERT_EQ(Device::Create(DeviceRequest{}, &def), IM_OK) << Detail();
  std::printf("[ info     ] default adapter: vendor 0x%04X device 0x%04X, warp %d, feature level 0x%X, video %d, "
              "NV12 render targets %d\n",
              def->vendor_id(), def->device_id(), def->warp() ? 1 : 0, static_cast<unsigned>(def->feature_level()),
              def->video_device() != nullptr ? 1 : 0, def->nv12_render_target() ? 1 : 0);
  EXPECT_GE(def->feature_level(), D3D_FEATURE_LEVEL_11_0);
  DeviceRequest by_luid;
  by_luid.adapter = DeviceRequest::Adapter::kLuid;
  by_luid.luid = def->luid();
  std::unique_ptr<Device> same;
  ASSERT_EQ(Device::Create(by_luid, &same), IM_OK) << Detail();
  EXPECT_EQ(same->luid(), def->luid());
  // An adapter that is gone (the LUID changes after a driver update, 04 §9.8).
  by_luid.luid = 0x7FFFFFFF12345678ull;
  std::unique_ptr<Device> none;
  EXPECT_EQ(Device::Create(by_luid, &none), IM_ERR_INTERNAL);
  EXPECT_EQ(none, nullptr);
  im::abi::ClearLastError();

  DeviceRequest warp;
  warp.adapter = DeviceRequest::Adapter::kWarp;
  std::unique_ptr<Device> w;
  ASSERT_EQ(Device::Create(warp, &w), IM_OK) << Detail();
  std::printf("[ info     ] WARP: feature level 0x%X, video %d, NV12 render targets %d\n",
              static_cast<unsigned>(w->feature_level()), w->video_device() != nullptr ? 1 : 0,
              w->nv12_render_target() ? 1 : 0);
}

// --------------------------------------------------------------------------------------------------- geometry

TEST(GpuGeometry, SpecExamplesAndFilters) {
  const auto geo = [](Size src, LayerBounds b) {
    return im::gpu::ComputeGeometry(im::video::ComputeOutputSize(src, b), src, src, src, Rotation::kIdentity);
  };
  ConvertGeometry g = geo({2560, 1440}, {2073600, 0, 0});
  EXPECT_TRUE(g.output == (Size{1920, 1080}) && g.dest == (Rect{0, 0, 1920, 1080}));
  EXPECT_EQ(g.filter, im::gpu::Filter::kBilinear);
  g = geo({1920, 1080}, {230400, 0, 0});
  EXPECT_TRUE(g.output == (Size{640, 360}));
  EXPECT_EQ(g.filter, im::gpu::Filter::kBox);
  g = geo({1280, 720}, {230400, 0, 0});
  EXPECT_EQ(g.filter, im::gpu::Filter::kBox); // exactly 2:1
  g = geo({3440, 1440}, {2073600, 0, 0});
  EXPECT_TRUE(g.output == (Size{2224, 930}));
  g = geo({120, 40}, {});
  EXPECT_TRUE(g.output == (Size{120, 64}) && g.dest == (Rect{0, 12, 120, 40}));

  EXPECT_EQ(im::gpu::RotationFromDxgi(0), Rotation::kIdentity);
  EXPECT_EQ(im::gpu::RotationFromDxgi(1), Rotation::kIdentity);
  EXPECT_EQ(im::gpu::RotationFromDxgi(2), Rotation::k90);
  EXPECT_EQ(im::gpu::RotationFromDxgi(3), Rotation::k180);
  EXPECT_EQ(im::gpu::RotationFromDxgi(4), Rotation::k270);
  EXPECT_TRUE(im::gpu::Upright(Size{1080, 1920}, Rotation::k90) == (Size{1920, 1080}));

  // The pointer's map and the blend model's inverse agree.
  for (const Rotation r : {Rotation::kIdentity, Rotation::k90, Rotation::k180, Rotation::k270}) {
    const Size region{300, 200};
    const Point p{12.5, 34.25};
    const Point q = wt::RegionToUpright(r, region, im::gpu::UprightToRegionPixels(r, region, p));
    EXPECT_NEAR(q.x, p.x, 1e-9);
    EXPECT_NEAR(q.y, p.y, 1e-9);
  }

  // The color model: white, black and red in BT.709 limited range.
  EXPECT_EQ(model::Code(model::ToYcc({1, 1, 1}).y), 235);
  EXPECT_EQ(model::Code(model::ToYcc({0, 0, 0}).y), 16);
  const model::Ycc red = model::ToYcc({1, 0, 0});
  EXPECT_EQ(model::Code(red.y), 63);
  EXPECT_EQ(model::Code(red.cb), 102);
  EXPECT_EQ(model::Code(red.cr), 240);
}

} // namespace
