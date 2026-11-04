# Golden access units for `annexb_test`

`annexb_test`'s `AnnexB.GoldenAccessUnits` (docs/m2/04-windows-engine.md §9.4, §18.1) runs each folder here as one
stream through one normalizer: `<folder>/<name>.h264` is one access unit exactly as an encoder produced it (Annex B,
before normalization), and name order is stream order (the first is an IDR with its headers). An optional
`<name>.h264.norm` next to it holds the expected normalized bytes. Other files, this one included, are ignored.

| Folder | Encoder | Stream |
|---|---|---|
| `videotoolbox/` | macOS VideoToolbox (Apple silicon), High | 640×360 at 15 fps, 300 kbit/s, IDRs at AUs 0 and 8, 12 AUs |
| `videotoolbox-cb/` | the same, Constrained Baseline | the same; its SPS says `42c01e`, which the normalizer rewrites to `42e01e` |

Each keyframe is SPS, PPS, an SEI (which the normalizer drops) and the IDR slice; the other AUs are one non-IDR slice.
They were made by a small VideoToolbox program (README WA44) that writes each AU the way FFmpeg's `h264_videotoolbox`
does with Annex B output: the format description's SPS and PPS in front of a keyframe, then the sample's NAL units with
4-byte start codes. The picture is synthetic (a moving ramp and a grid of glyph-like blocks): no screen content.

The Windows encoders' folders (`nvenc/`, `qsv/`, `amf/`, `mf-hw/`, `mf-sw/`) come from the owner's PC with
`isshoni-enginetest capture -encoder <token> -dump-aus 12 -out DIR` (04 §14), which writes q's first AUs to
`DIR/aus/<token>/`. The tool reads them from the engine, so they are the normalizer's output already: the test then
checks that every invariant holds and that normalizing again changes nothing (idempotence).
