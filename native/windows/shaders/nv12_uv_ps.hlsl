// The shader converter's chroma pass (docs/m2/04-windows-engine.md section 8.3): the half-size Cb/Cr plane of the NV12
// output through an R8G8_UNORM render target view, BT.709 limited range, 128 outside the content. Each chroma pixel
// is sited at its 2x2 block's left luma column, between the block's two rows
// (DXGI_COLOR_SPACE_YCBCR_STUDIO_G22_LEFT_P709).
#include "convert.hlsli"

float2 main(float4 pos : SV_Position) : SV_Target {
  float2 p = float2(2.0 * pos.x - 0.5, 2.0 * pos.y);
  if (!InsideDest(p)) {
    return float2(128.0, 128.0) / 255.0;
  }
  float3 rgb = Fetch(p);
  float y = dot(rgb, kLuma);
  float2 c = float2((rgb.b - y) / 1.8556, (rgb.r - y) / 1.5748); // / 2(1 - Kb), / 2(1 - Kr)
  return (128.0 + 224.0 * c) / 255.0;
}
