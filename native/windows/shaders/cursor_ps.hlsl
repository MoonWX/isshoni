// The pointer image, point-sampled one texel per pixel (docs/m2/04-windows-engine.md section 7.8); the blend state does
// straight alpha (SRC_ALPHA, INV_SRC_ALPHA).
Texture2D<float4> g_shape : register(t0);
SamplerState g_point : register(s0); // MIN_MAG_MIP_POINT, CLAMP

float4 main(float4 pos : SV_Position, float2 uv : TEXCOORD0) : SV_Target {
  return g_shape.SampleLevel(g_point, uv, 0.0);
}
