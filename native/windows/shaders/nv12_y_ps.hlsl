// The shader converter's luma pass (docs/m2/04-windows-engine.md section 8.3): the Y plane of the NV12 output through
// an R8_UNORM render target view, BT.709 limited range, black (16) outside the content.
#include "convert.hlsli"

float main(float4 pos : SV_Position) : SV_Target {
  float y = 16.0 / 255.0; // one return, as Fetch has (fxc /WX, X4000)
  if (InsideDest(pos.xy)) {
    y = (16.0 + 219.0 * dot(Fetch(pos.xy), kLuma)) / 255.0;
  }
  return y;
}
