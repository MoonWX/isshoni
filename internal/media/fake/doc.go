// Package fake is the fake media source of the tests and the load test (docs/m1/02-sfu.md §15.1): a test pattern
// and a tone, shaped like the native capture engine's output, paced in real time and pulled like im_next_packet.
//
// The Synthetic mode makes codec-shaped H.264: a keyframe is a real SPS (the configured profile, a level that fits
// the layer, its size) and PPS plus one IDR slice NAL unit, a delta frame one non-IDR slice NAL unit. Right after its
// NAL header byte, every slice carries an 18-byte Marker (magic "ISHN", rid, flags, frame index, capture time), then
// seeded filler bytes, all with standard emulation prevention. The SFU reads only NAL headers and the SPS, so these
// streams give the SFU accurate packet rates, sizes and keyframe bursts at almost no CPU. They do not decode.
//
// Audio is one Opus-shaped packet every 20 ms: a valid Opus packet (code 3, one CELT frame) whose padding, which
// decoders ignore, holds the same Marker. Flash frames (one per layer) and the beep packet share one capture instant
// every FlashEvery, which is how the A/V checks pair them.
//
// The Decodable mode (a pure-Go I_PCM/P_Skip encoder that browsers decode) and the committed Opus asset are README
// slice S22; until then New refuses Mode Decodable with ErrNotImplemented and audio stays Opus-shaped filler.
//
// This is test code: cmd/isshoni never imports it and no release artifact contains it (06's build checks that).
package fake
