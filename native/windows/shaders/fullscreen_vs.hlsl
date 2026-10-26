// A triangle that covers the whole viewport, without a vertex buffer or input layout: Draw(3, 0) with SV_VertexID
// 0, 1, 2 (the shader converter's passes and the test pattern, docs/m2/04-windows-engine.md section 8.3).
float4 main(uint id : SV_VertexID) : SV_Position {
  float2 t = float2(float((id << 1u) & 2u), float(id & 2u));
  return float4(t.x * 2.0 - 1.0, 1.0 - t.y * 2.0, 0.0, 1.0);
}
