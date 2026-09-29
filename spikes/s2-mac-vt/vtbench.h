// vtbench.h: small C API over the VideoToolbox benchmark (vtbench.m).
//
// Pull-based: vtb_run() drives the encoders on its own native thread and blocks until
// every frame is back. VT output callbacks only write into preallocated native records
// and a lock-protected byte buffer; they never call into Go. Go then pulls the frame
// records, the Annex B stream and a JSON description of each layer.

#ifndef VTBENCH_H
#define VTBENCH_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

enum { VTB_FMT_BGRA = 0, VTB_FMT_NV12 = 1 };
enum { VTB_MODE_PACED = 0, VTB_MODE_THROUGHPUT = 1 };

// Frame record flags.
enum {
    VTB_F_KEYFRAME      = 1 << 0, // output sample is a sync sample
    VTB_F_DROPPED       = 1 << 1, // kVTEncodeInfo_FrameDropped (submit or callback)
    VTB_F_NO_OUTPUT     = 1 << 2, // callback came without a sample buffer
    VTB_F_FORCED_KF     = 1 << 3, // submitted with kVTEncodeFrameOptionKey_ForceKeyFrame
    VTB_F_CALLBACK      = 1 << 4, // output callback seen
    VTB_F_SUBMIT_FAILED = 1 << 5, // VTCompressionSessionEncodeFrame returned an error
    VTB_F_SUBMITTED     = 1 << 6, // record is in use
    VTB_F_TRUNCATED     = 1 << 7, // stream capture cap reached, bytes not kept
};

typedef struct {
    int32_t width, height;
    int32_t fps;               // frame rate: pts timescale and pacing
    int32_t expected_fps;      // ExpectedFrameRate (0: not set)
    int64_t bitrate;           // AverageBitRate, bits/s
    double data_rate_factor;   // >0: DataRateLimits = [bitrate*factor/8 bytes, 1 s]; 0: not set
    double keyint_sec;         // MaxKeyFrameIntervalDuration (and MaxKeyFrameInterval = keyint*fps); 0: not set
    int32_t low_latency_rc;    // kVTVideoEncoderSpecification_EnableLowLatencyRateControl
    int32_t prioritize_speed;  // kVTCompressionPropertyKey_PrioritizeEncodingSpeedOverQuality
    int32_t real_time;         // kVTCompressionPropertyKey_RealTime
    int32_t require_hw;        // kVTVideoEncoderSpecification_RequireHardwareAcceleratedVideoEncoder
    int32_t software;          // EnableHardwareAcceleratedVideoEncoder = false (Apple's software H.264)
} vtb_enc_config;

typedef struct {
    int32_t mode;              // VTB_MODE_*
    int32_t frames;            // paced: frames to submit (incl. warm-up); throughput: record capacity
    double max_seconds;        // throughput: stop submitting after this long
    int32_t inflight;          // throughput: max frames in flight
    int32_t force_kf_frame;    // full-layer frame index submitted with ForceKeyFrame (-1: none)
    int32_t encode_full;       // 0: don't encode the full layer (preview-only baseline)
    int32_t preview;           // 1: add a second layer, downscaled with VTPixelTransferSession
    vtb_enc_config preview_cfg;
    int64_t stream_cap_bytes;  // per-layer cap on captured Annex B bytes
} vtb_run_config;

// One record per submitted frame. Times are ns since the run started.
// The submit thread only writes target/submit/return/xfer, submit_status and sflags; the
// output callback only writes done/stream/bytes, cb_status and cflags (no shared RMW).
typedef struct {
    int64_t target_ns;     // paced: scheduled submit time
    int64_t submit_ns;     // just before VTCompressionSessionEncodeFrame
    int64_t return_ns;     // VTCompressionSessionEncodeFrame returned
    int64_t done_ns;       // output callback entered (0: none)
    int64_t xfer_ns;       // preview layer: VTPixelTransferSessionTransferImage duration
    int64_t stream_off;    // offset of this access unit in the layer's Annex B stream (-1: none)
    int32_t stream_len;    // Annex B bytes (incl. in-band SPS/PPS on keyframes)
    int32_t bytes;         // encoded payload bytes as delivered by VT (AVCC)
    int32_t frame_index;   // source (full-layer) frame index
    int32_t submit_status; // OSStatus from EncodeFrame (or the preview downscale)
    int32_t cb_status;     // OSStatus passed to the output callback
    int32_t sflags;        // VTB_F_* set by the submit thread
    int32_t cflags;        // VTB_F_* set by the output callback
    int32_t _pad;
} vtb_frame;

typedef struct {
    int32_t decoded;        // frames that came out of VTDecompressionSession
    int32_t failed;         // decode calls or callbacks with an error
    int32_t first_error;    // first OSStatus seen
    int32_t width, height;  // decoded image size
    int32_t delayed;        // frames output after a later frame was submitted (decoder reorder buffering)
    int32_t psnr_frames;    // frames compared against the source ring (NV12 sources only)
    int32_t format_changes; // format descriptions created (SPS/PPS changes)
    double psnr_y_mean;     // mean of per-frame luma PSNR (dB)
    double psnr_y_min;      // worst frame
    double decode_ms;       // wall time for the whole check
} vtb_decode_result;

typedef struct vtb_bench vtb_bench;

// Pre-renders a ring of screen-like frames (IOSurface-backed, from a CVPixelBufferPool).
// ring_max caps the ring length; ring_bytes_max caps the ring's memory.
vtb_bench *vtb_open(int32_t width, int32_t height, int32_t fmt, int32_t ring_max,
                    int64_t ring_bytes_max, char *err, int32_t errlen);
int32_t vtb_ring_len(vtb_bench *b);
double vtb_prerender_ms(vtb_bench *b);

// Runs one benchmark. Previous results are discarded. Returns 0, or an OSStatus if a
// session could not be created (details in the layer JSON).
int32_t vtb_run(vtb_bench *b, const vtb_enc_config *full, const vtb_run_config *run);

// Results of the last run. Layer 0 is the full layer, layer 1 the preview (if any).
int32_t vtb_layer_count(vtb_bench *b);
char *vtb_layer_info_json(vtb_bench *b, int32_t layer); // free with vtb_free
int32_t vtb_layer_frames(vtb_bench *b, int32_t layer, const vtb_frame **out);
int64_t vtb_layer_stream(vtb_bench *b, int32_t layer, const uint8_t **out);

// Decodes Annex B access units with VTDecompressionSession. If psnr != 0 and the ring is
// NV12, each decoded frame is compared with ring[au_frame[i] % ring_len].
int32_t vtb_decode_check(vtb_bench *b, const uint8_t *annexb, const int64_t *au_off,
                         const int32_t *au_len, const int32_t *au_frame, int32_t n, int32_t psnr,
                         vtb_decode_result *out);

void vtb_close(vtb_bench *b);

// Process-wide NSProcessInfo activity (UserInitiated, optionally LatencyCritical, which
// turns off timer coalescing / App Nap). Returns a token for vtb_end_activity.
void *vtb_begin_activity(int32_t latency_critical);
void vtb_end_activity(void *token);

char *vtb_encoders_json(void); // H.264 encoders from VTCopyVideoEncoderList
char *vtb_system_json(void);   // model, OS version, thermal state (no host or user names)
void vtb_free(void *p);

#ifdef __cplusplus
}
#endif

#endif
