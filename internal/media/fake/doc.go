// Package fake is the fake media source of the tests and the load test (docs/m1/02-sfu.md §15.1): a test pattern
// and a tone, shaped like the native capture engine's output, paced in real time and pulled like im_next_packet.
//
// The Synthetic mode makes codec-shaped H.264: a keyframe is a real SPS (the configured profile, a level that fits
// the layer, its size) and PPS plus one IDR slice NAL unit, a delta frame one non-IDR slice NAL unit. Right after its
// NAL header byte, every slice carries an 18-byte Marker (magic "ISHN", rid, flags, frame index, capture time), then
// seeded filler bytes, all with standard emulation prevention. The SFU reads only NAL headers and the SPS, so these
// streams give the SFU accurate packet rates, sizes and keyframe bursts at almost no CPU. They do not decode.
//
// The Decodable mode (decodable.go) is a tiny pure-Go encoder written from the H.264 spec: Constrained Baseline
// ("42e0") with I_PCM and P_Skip macroblocks only, deterministic, any even size, license-clean and CGO-free. Browsers
// decode it: an IDR frame is all I_PCM, a delta frame codes as I_PCM only the macroblocks that changed (a moving box,
// a frame counter, a flash square and the marker macroblock, whose samples carry the same Marker) and skips the
// rest. Its default layers are f 640×360@30 and q 320×180@15 (DefaultDecodableLayers).
//
// Audio is one Opus packet every 20 ms from a committed asset (opus.go, testdata/tone.opus, made once with opusenc
// by testdata/gen-opus.sh): a 440 Hz tone with a 1 kHz beep in the first 100 ms of every second, looped. Each packet
// is the asset's packet reframed as code 3 with the same Marker in its padding, which decoders ignore. Flash frames
// (one per layer) and Beep packets share one capture instant every FlashEvery, which is how the A/V checks pair them.
// ParseVideoMarker and ParseAudioMarker read the Markers back from RTP payloads (sfutest.Viewer).
//
// This is test code: cmd/isshoni never imports it and no release artifact contains it (06's build checks that).
package fake
