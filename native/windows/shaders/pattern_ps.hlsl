// The GPU test pattern (docs/m2/04-windows-engine.md section 2 "Test builds"): bands of color bars, a grey ramp, a
// moving bar, a frame counter and the flash box. src/gpu/pattern_layout.h describes the look and computes every number
// here; its PatternPixel is the CPU model of this shader, pixel for pixel.
cbuffer Pattern : register(b0) {
  uint4 g_rows;  // where the bars, the ramp, the moving bar and the counter end (rows)
  uint4 g_size;  // width, height, the flash box's left edge, the moving bar's width
  uint4 g_frame; // the moving bar's left edge, the frame index's low 16 bits, flash (0 or 1), 0
};

float3 Grey(uint v) {
  float f = float(v) / 255.0;
  return float3(f, f, f);
}

float4 main(float4 pos : SV_Position) : SV_Target {
  uint x = uint(pos.x);
  uint y = uint(pos.y);
  float3 c = Grey(32u);
  if (y < g_rows.x) {
    // Bar i's R, G and B are bit i of 0x33, 0x0F and 0x55: white, yellow, cyan, green, magenta, red, blue, black.
    uint bar = min(x * 8u / g_size.x, 7u);
    c = float3(float((0x33u >> bar) & 1u), float((0x0Fu >> bar) & 1u), float((0x55u >> bar) & 1u));
  } else if (y < g_rows.y) {
    c = Grey(x * 255u / (g_size.x - 1u));
  } else if (y < g_rows.z) {
    if (x >= g_frame.x && x < g_frame.x + g_size.w) {
      c = Grey(255u);
    }
  } else if (y < g_rows.w) {
    uint block = min(x * 16u / g_size.x, 15u);
    c = Grey(((g_frame.y >> (15u - block)) & 1u) * 255u);
  } else if (x >= g_size.z) {
    c = Grey(g_frame.z * 255u);
  }
  return float4(c, 1.0);
}
