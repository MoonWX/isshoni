// The pointer's quad (docs/m2/04-windows-engine.md section 7.8): Draw(4, 0) as a triangle strip without a vertex
// buffer. src/gpu/cursor.cpp computes the corners, already turned for the slot's rotation.
cbuffer Quad : register(b0) {
  float4 g_corner[4]; // xy: clip-space position, zw: texture coordinates; the pointer image's TL, TR, BL, BR
};

struct VsOut {
  float4 pos : SV_Position;
  float2 uv : TEXCOORD0;
};

VsOut main(uint id : SV_VertexID) {
  float4 c = g_corner[id];
  VsOut o;
  o.pos = float4(c.xy, 0.0, 1.0);
  o.uv = c.zw;
  return o;
}
