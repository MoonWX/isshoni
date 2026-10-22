// The shader converter's shared part (docs/m2/04-windows-engine.md section 8.3), included by nv12_y_ps.hlsl and
// nv12_uv_ps.hlsl: BT.709 limited range from the BGRA slot texture, the bilinear filter or the preview's 2x2 box over
// bilinear taps, the rotation and the letterbox. src/gpu/geometry.h (MakeConvertConstants) fills the constants and
// src/gpu/cpu_model.h is the CPU model of exactly this code; keep the three in step.
//
// Not a shader of its own: the build compiles only *_vs.hlsl, *_ps.hlsl and *_cs.hlsl (native/windows/CMakeLists.txt),
// and each of them depends on every shaders/*.hlsli, so a change here recompiles them all.

cbuffer Convert : register(b0) {
  float4 g_dest;      // x, y, width, height of the content in output (luma) pixels
  float2 g_src_scale; // the content region in normalized texture coordinates
  float2 g_texel;     // one texel in normalized texture coordinates
  uint g_rotation;    // 0, 1 (90 degrees clockwise), 2 (180 degrees), 3 (270 degrees)
  uint g_box;         // 1: the mean of four taps half a texel to each side
  uint2 g_pad;
};

Texture2D<float4> g_src : register(t0);
SamplerState g_linear : register(s0); // MIN_MAG_MIP_LINEAR, CLAMP

static const float3 kLuma = float3(0.2126, 0.7152, 0.0722);

bool InsideDest(float2 p) {
  return p.x >= g_dest.x && p.y >= g_dest.y && p.x < g_dest.x + g_dest.z && p.y < g_dest.y + g_dest.w;
}

// A point of the upright content, normalized, to the captured region, normalized (geometry.h's UprightToRegion).
float2 ToRegion(float2 u) {
  float2 r = u;
  if (g_rotation == 1u) {
    r = float2(u.y, 1.0 - u.x);
  } else if (g_rotation == 2u) {
    r = float2(1.0 - u.x, 1.0 - u.y);
  } else if (g_rotation == 3u) {
    r = float2(1.0 - u.y, u.x);
  }
  return r;
}

// One bilinear tap, its coordinates clamped to the content's texel centers: a content smaller than its texture never
// reads past its edge.
float3 Tap(float2 st) {
  float2 lo = 0.5 * g_texel;
  float2 hi = max(lo, g_src_scale - 0.5 * g_texel);
  return g_src.SampleLevel(g_linear, clamp(st, lo, hi), 0.0).rgb;
}

// The R'G'B' at an output position p (luma pixels) inside the destination rectangle.
float3 Fetch(float2 p) {
  float2 st = ToRegion((p - g_dest.xy) / g_dest.zw) * g_src_scale;
  if (g_box != 0u) {
    float2 h = 0.5 * g_texel;
    return 0.25 * (Tap(st - h) + Tap(st + float2(h.x, -h.y)) + Tap(st + float2(-h.x, h.y)) + Tap(st + h));
  }
  return Tap(st);
}
