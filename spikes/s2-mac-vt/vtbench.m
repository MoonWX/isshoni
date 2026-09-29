// vtbench.m: VideoToolbox H.264 encode benchmark for spike S2 (macOS).
//
// - Pre-renders a ring of screen-like frames (scrolling code + document text, a noisy
//   "video" window, a moving cursor) into IOSurface-backed CVPixelBuffers, BGRA or NV12.
// - Encodes them at a paced 60 fps (mach_wait_until) or as fast as possible, optionally
//   with a second 360p15 layer made by VTPixelTransferSession.
// - The output callback timestamps each frame and converts AVCC to Annex B (SPS/PPS in
//   band before each IDR) into a lock-protected buffer. Nothing here calls into Go.

#import <CoreFoundation/CoreFoundation.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#import <Foundation/Foundation.h>
#import <IOKit/ps/IOPowerSources.h>
#import <IOKit/ps/IOPSKeys.h>
#import <IOSurface/IOSurface.h>
#import <VideoToolbox/VideoToolbox.h>

#include <mach/mach.h>
#include <mach/mach_time.h>
#include <mach/thread_policy.h>
#include <math.h>
#include <os/lock.h>
#include <pthread.h>
#include <stdatomic.h>
#include <string.h>
#include <sys/resource.h>
#include <sys/sysctl.h>

#include "vtbench.h"

// ---------------------------------------------------------------------------------------
// Time

static mach_timebase_info_data_t g_tb;

static void tb_init(void) {
    static dispatch_once_t once;
    dispatch_once(&once, ^{
      mach_timebase_info(&g_tb);
    });
}

static inline int64_t ticks_to_ns(uint64_t t) {
    return (int64_t)((__uint128_t)t * g_tb.numer / g_tb.denom);
}

static inline uint64_t ns_to_ticks(int64_t ns) {
    return (uint64_t)((__uint128_t)ns * g_tb.denom / g_tb.numer);
}

static inline double ms_since(uint64_t start) {
    return (double)ticks_to_ns(mach_absolute_time() - start) / 1e6;
}

static NSString *fourcc(uint32_t v) {
    char s[5] = {(char)(v >> 24), (char)(v >> 16), (char)(v >> 8), (char)v, 0};
    for (int i = 0; i < 4; i++) {
        if (s[i] < 32 || s[i] > 126) return [NSString stringWithFormat:@"0x%08x", v];
    }
    return [NSString stringWithUTF8String:s];
}

static char *json_dup(id obj) {
    NSError *err = nil;
    NSData *d = [NSJSONSerialization dataWithJSONObject:obj options:NSJSONWritingSortedKeys error:&err];
    if (!d) {
        NSString *s = [NSString stringWithFormat:@"{\"json_error\":\"%@\"}", err.localizedDescription];
        return strdup(s.UTF8String);
    }
    char *out = malloc(d.length + 1);
    memcpy(out, d.bytes, d.length);
    out[d.length] = 0;
    return out;
}

// Converts a CF property value into something NSJSONSerialization accepts.
static id json_value(CFTypeRef v) {
    if (!v) return [NSNull null];
    id o = (__bridge id)v;
    if ([o isKindOfClass:[NSNumber class]] || [o isKindOfClass:[NSString class]]) return o;
    if ([o isKindOfClass:[NSArray class]]) {
        NSMutableArray *a = [NSMutableArray array];
        for (id e in (NSArray *)o) [a addObject:json_value((__bridge CFTypeRef)e)];
        return a;
    }
    if ([o isKindOfClass:[NSDictionary class]]) {
        NSMutableDictionary *d = [NSMutableDictionary dictionary];
        [(NSDictionary *)o enumerateKeysAndObjectsUsingBlock:^(id k, id val, BOOL *stop) {
          d[[k description]] = json_value((__bridge CFTypeRef)val);
        }];
        return d;
    }
    return [o description];
}

// ---------------------------------------------------------------------------------------
// Synthetic screen content

static inline uint32_t hash32(uint32_t x) {
    x ^= x >> 16;
    x *= 0x7feb352dU;
    x ^= x >> 15;
    x *= 0x846ca68bU;
    x ^= x >> 16;
    return x;
}

static inline uint32_t xs32(uint32_t s) {
    s ^= s << 13;
    s ^= s >> 17;
    s ^= s << 5;
    return s;
}

#define NGLYPH 96
static uint8_t g_glyph[NGLYPH][9]; // 5 columns x 9 rows, bit 4 = leftmost column

static void glyphs_init(void) {
    static dispatch_once_t once;
    dispatch_once(&once, ^{
      for (int g = 0; g < NGLYPH; g++) {
          uint32_t h = hash32((uint32_t)g * 2654435761U + 17);
          memset(g_glyph[g], 0, 9);
          int strokes = 2 + (int)(h % 3);
          for (int s = 0; s < strokes; s++) {
              h = xs32(h);
              int kind = (int)(h % 4);
              int a = (int)((h >> 4) % 5), r0 = 1 + (int)((h >> 8) % 4), r1 = 5 + (int)((h >> 12) % 4);
              if (kind == 0) { // vertical stroke
                  for (int r = r0; r <= r1; r++) g_glyph[g][r] |= (uint8_t)(0x10 >> a);
              } else if (kind == 1) { // horizontal stroke
                  int row = 2 + (int)((h >> 16) % 7), c0 = (int)((h >> 20) % 3), c1 = 2 + (int)((h >> 24) % 3);
                  for (int c = c0; c <= c1; c++) g_glyph[g][row] |= (uint8_t)(0x10 >> c);
              } else if (kind == 2) { // diagonal
                  for (int r = r0; r <= r1; r++) g_glyph[g][r] |= (uint8_t)(0x10 >> ((a + r - r0) % 5));
              } else { // bowl
                  for (int r = 4; r <= 8; r++) g_glyph[g][r] |= (r == 4 || r == 8) ? 0x0e : 0x11;
              }
          }
      }
    });
}

typedef struct {
    int W, H;
    int cw, ch;       // text cell size
    int gw, gh, gtop; // glyph box inside a cell
    int titleH, sbW, edX1, vidY0;
    int scroll1, scroll2; // px per frame (editor, document)
} layout_t;

static layout_t make_layout(int W, int H) {
    layout_t L;
    L.W = W;
    L.H = H;
    L.cw = (int)lround(H / 120.0);
    if (L.cw < 6) L.cw = 6;
    L.ch = L.cw * 2;
    L.gw = L.cw - (L.cw / 5 > 1 ? L.cw / 5 : 1);
    L.gh = (L.ch * 5) / 8;
    L.gtop = (L.ch - L.gh) / 2;
    L.titleH = (H * 3) / 100;
    L.sbW = (W * 14) / 100;
    L.edX1 = (W * 55) / 100;
    L.vidY0 = (H * 45) / 100;
    L.scroll1 = (int)lround(H / 360.0); // 1440p: 4 px/frame = 240 px/s
    if (L.scroll1 < 1) L.scroll1 = 1;
    L.scroll2 = L.scroll1 / 2 > 0 ? L.scroll1 / 2 : 1;
    return L;
}

#define MAXCOLS 512

// Deterministic "text" for a line: glyph index+1 per column (0 = blank) and a colour index.
static void line_layout(uint32_t line, uint32_t salt, int ncols, int indent, int ncolors, uint8_t *chars,
                        uint8_t *cidx) {
    if (ncols > MAXCOLS) ncols = MAXCOLS;
    memset(chars, 0, (size_t)ncols);
    memset(cidx, 0, (size_t)ncols);
    uint32_t h = hash32(line * 0x9E3779B1U ^ salt);
    if (h % 100 < 12) return; // blank line
    int col = indent ? (int)((h >> 8) % 5) * 4 : 0;
    int maxlen = ncols < 110 ? ncols : 110;
    int len = 12 + (int)((h >> 16) % (uint32_t)(maxlen > 13 ? maxlen - 12 : 1));
    uint32_t s = h | 1;
    while (col < len && col < ncols) {
        s = xs32(s);
        int wl = 1 + (int)(s % 9);
        uint8_t color = (uint8_t)((s >> 8) % (uint32_t)ncolors);
        for (int k = 0; k < wl && col < ncols; k++) {
            s = xs32(s);
            chars[col] = (uint8_t)(1 + 10 + s % (NGLYPH - 10)); // glyphs 0..9 are digits
            cidx[col] = color;
            col++;
        }
        col++;
    }
}

static inline uint32_t rgb(int r, int g, int b) {
    if (r < 0) r = 0;
    if (r > 255) r = 255;
    if (g < 0) g = 0;
    if (g > 255) g = 255;
    if (b < 0) b = 0;
    if (b > 255) b = 255;
    return 0xFF000000U | ((uint32_t)r << 16) | ((uint32_t)g << 8) | (uint32_t)b;
}

// Draws one row of text cells between x0 and x1. yin = row within the text line.
static void text_row(uint32_t *row, int x0, int x1, int yin, const layout_t *L, const uint8_t *chars,
                     const uint8_t *cidx, const uint32_t *pal, uint32_t bg) {
    int gy = yin - L->gtop;
    int by = (gy >= 0 && gy < L->gh) ? (gy * 9) / L->gh : -1;
    for (int x = x0; x < x1; x++) {
        int rel = x - x0, col = rel / L->cw, xin = rel % L->cw;
        uint8_t c = col < MAXCOLS ? chars[col] : 0;
        uint32_t px = bg;
        if (c && by >= 0 && xin < L->gw) {
            int bx = (xin * 5) / L->gw;
            if ((g_glyph[c - 1][by] >> (4 - bx)) & 1) px = pal[cidx[col]];
        }
        row[x] = px;
    }
}

static void digits_into(uint8_t *chars, uint8_t *cidx, int start, int width, uint32_t value, uint8_t color) {
    for (int i = width - 1; i >= 0; i--) {
        chars[start + i] = (uint8_t)(1 + value % 10);
        cidx[start + i] = color;
        value /= 10;
        if (value == 0) break;
    }
}

static void render_rows(uint32_t *base, size_t stride_px, const layout_t *L, int f, int y0, int y1) {
    static const uint32_t code_pal[7] = {0xFFD4D4D4, 0xFF569CD6, 0xFFCE9178, 0xFF6A9955,
                                         0xFFC586C0, 0xFF4EC9B0, 0xFFDCDCAA};
    static const uint32_t doc_pal[3] = {0xFF111111, 0xFF1A0DAB, 0xFF444444};
    static const uint32_t side_pal[2] = {0xFFCCCCCC, 0xFF8FB8E8};
    static const uint32_t title_pal[2] = {0xFFE0E0E0, 0xFFFFC857};
    uint8_t chars[MAXCOLS], cidx[MAXCOLS];
    const int W = L->W, H = L->H;
    const float sx = 1080.0f / (float)H;
    const int vx0 = L->edX1, vw = W - vx0, vh = H - L->vidY0;
    const int bs = vh / 4;
    // bouncing box position (triangle waves)
    int spanx = vw - bs > 1 ? vw - bs : 1, spany = vh - bs > 1 ? vh - bs : 1;
    int px = (f * (int)(9 / sx + 1)) % (2 * spanx);
    if (px >= spanx) px = 2 * spanx - px;
    int py = (f * (int)(5 / sx + 1)) % (2 * spany);
    if (py >= spany) py = 2 * spany - py;
    // cursor
    int curX = (int)(W * (0.5 + 0.42 * sin(f * 0.071))), curY = (int)(H * (0.5 + 0.42 * sin(f * 0.043 + 1.0)));
    int curH = L->ch;

    for (int y = y0; y < y1; y++) {
        uint32_t *row = base + (size_t)y * stride_px;
        if (y < L->titleH) {
            int ncols = W / L->cw;
            line_layout(4242, 0x51u, ncols, 0, 1, chars, cidx);
            if (ncols > 8) digits_into(chars, cidx, ncols - 8, 7, (uint32_t)f, 1);
            int yin = (y * L->ch) / (L->titleH > 0 ? L->titleH : 1);
            text_row(row, 0, W, yin, L, chars, cidx, title_pal, 0xFF3C3C3C);
            continue;
        }
        int yy = y - L->titleH;
        // sidebar: static file tree
        {
            uint32_t line = (uint32_t)(yy / L->ch);
            line_layout(line, 0xA11CEu, L->sbW / L->cw, 1, 2, chars, cidx);
            text_row(row, 0, L->sbW, yy % L->ch, L, chars, cidx, side_pal, 0xFF252526);
        }
        // editor: line numbers + code, scrolling
        {
            int cy = yy + f * L->scroll1;
            uint32_t line = (uint32_t)(cy / L->ch);
            int ncols = (L->edX1 - L->sbW) / L->cw;
            uint8_t lc[MAXCOLS], li[MAXCOLS];
            line_layout(line, 0xC0DEu, ncols, 1, 7, lc, li);
            // shift code right by 6 columns for the gutter
            memset(chars, 0, sizeof(chars));
            memset(cidx, 0, sizeof(cidx));
            for (int c = 0; c + 6 < ncols && c + 6 < MAXCOLS; c++) {
                chars[c + 6] = lc[c];
                cidx[c + 6] = li[c];
            }
            digits_into(chars, cidx, 0, 5, line + 1, 0);
            text_row(row, L->sbW, L->edX1, cy % L->ch, L, chars, cidx, code_pal, 0xFF1E1E1E);
            // gutter digits in grey: recolour columns 0..4
            for (int x = L->sbW; x < L->sbW + 5 * L->cw && x < L->edX1; x++)
                if (row[x] != 0xFF1E1E1E) row[x] = 0xFF858585;
        }
        if (y < L->vidY0) {
            // document window: dark text on white, slower scroll
            int cy = yy + f * L->scroll2;
            uint32_t line = (uint32_t)(cy / L->ch);
            line_layout(line, 0xD0C5u, (W - L->edX1) / L->cw - 2, 0, 3, chars, cidx);
            text_row(row, L->edX1 + L->cw, W, cy % L->ch, L, chars, cidx, doc_pal, 0xFFFFFFFF);
            for (int x = L->edX1; x < L->edX1 + L->cw && x < W; x++) row[x] = 0xFFFFFFFF;
        } else {
            // video window: moving plasma + per-frame grain + bouncing checkerboard box
            float v = (float)(y - L->vidY0) * sx, t = (float)f;
            int by = y - L->vidY0 - py;
            for (int x = vx0; x < W; x++) {
                float u = (float)(x - vx0) * sx;
                float p = sinf(u * 0.021f + t * 0.13f) + sinf(v * 0.027f - t * 0.09f) +
                          sinf((u + v) * 0.013f + t * 0.07f);
                int r = 128 + (int)(90.0f * sinf(p * 1.3f));
                int g = 128 + (int)(90.0f * sinf(p * 1.7f + 2.1f));
                int b = 128 + (int)(90.0f * sinf(p * 2.3f + 4.2f));
                int bx = x - vx0 - px;
                if (bx >= 0 && bx < bs && by >= 0 && by < bs) {
                    int sq = bs / 8 > 0 ? bs / 8 : 1;
                    if (((bx / sq) + (by / sq)) & 1) {
                        r = 250; g = 210; b = 40;
                    } else {
                        r = 30; g = 60; b = 200;
                    }
                }
                uint32_t n = hash32((uint32_t)x * 73856093U ^ (uint32_t)y * 19349663U ^ (uint32_t)f * 83492791U);
                r += (int)(n & 31) - 16;
                g += (int)((n >> 5) & 31) - 16;
                b += (int)((n >> 10) & 31) - 16;
                row[x] = rgb(r, g, b);
            }
        }
        // cursor: white arrow with black outline
        int cr = y - curY;
        if (cr >= 0 && cr < curH) {
            int wdt = cr / 2 + 1;
            for (int k = 0; k <= wdt; k++) {
                int x = curX + k;
                if (x < 0 || x >= W) continue;
                row[x] = (k == 0 || k == wdt || cr == curH - 1) ? 0xFF000000 : 0xFFFFFFFF;
            }
        }
    }
}

static void render_frame(CVPixelBufferRef pb, const layout_t *L, int f) {
    CVPixelBufferLockBaseAddress(pb, 0);
    uint32_t *base = CVPixelBufferGetBaseAddress(pb);
    size_t stride_px = CVPixelBufferGetBytesPerRow(pb) / 4;
    const int bands = 64;
    dispatch_apply(bands, DISPATCH_APPLY_AUTO, ^(size_t i) {
      int y0 = (int)(i * (size_t)L->H / bands), y1 = (int)((i + 1) * (size_t)L->H / bands);
      render_rows(base, stride_px, L, f, y0, y1);
    });
    CVPixelBufferUnlockBaseAddress(pb, 0);
}

static void set_color_attachments(CVPixelBufferRef pb) {
    CVBufferSetAttachment(pb, kCVImageBufferColorPrimariesKey, kCVImageBufferColorPrimaries_ITU_R_709_2,
                          kCVAttachmentMode_ShouldPropagate);
    CVBufferSetAttachment(pb, kCVImageBufferTransferFunctionKey, kCVImageBufferTransferFunction_ITU_R_709_2,
                          kCVAttachmentMode_ShouldPropagate);
    CVBufferSetAttachment(pb, kCVImageBufferYCbCrMatrixKey, kCVImageBufferYCbCrMatrix_ITU_R_709_2,
                          kCVAttachmentMode_ShouldPropagate);
}

static void xfer_set_709(VTPixelTransferSessionRef x) {
    VTSessionSetProperty(x, kVTPixelTransferPropertyKey_DestinationColorPrimaries,
                         kCMFormatDescriptionColorPrimaries_ITU_R_709_2);
    VTSessionSetProperty(x, kVTPixelTransferPropertyKey_DestinationTransferFunction,
                         kCMFormatDescriptionTransferFunction_ITU_R_709_2);
    VTSessionSetProperty(x, kVTPixelTransferPropertyKey_DestinationYCbCrMatrix,
                         kCMFormatDescriptionYCbCrMatrix_ITU_R_709_2);
}

static NSDictionary *pb_attrs(int w, int h, OSType pf) {
    return @{
        (id)kCVPixelBufferPixelFormatTypeKey : @(pf),
        (id)kCVPixelBufferWidthKey : @(w),
        (id)kCVPixelBufferHeightKey : @(h),
        (id)kCVPixelBufferIOSurfacePropertiesKey : @{},
    };
}

// ---------------------------------------------------------------------------------------
// Objects

@interface VTBLayer : NSObject {
  @public
    vtb_enc_config cfg;
    OSType inputFormat;
    VTCompressionSessionRef session;
    uint64_t t0;
    vtb_frame *frames;
    _Atomic uint8_t *released;
    int32_t cap;
    int32_t used;
    _Atomic int32_t callbacks;
    os_unfair_lock lock;
    uint8_t *stream;
    int64_t streamLen, streamCapacity, streamCapBytes;
    uint8_t *scratch;
    size_t scratchCap;
    dispatch_semaphore_t inflight;
    NSMutableDictionary *info;
    NSMutableArray *setLog;
}
@end

@implementation VTBLayer
- (instancetype)initWithCapacity:(int32_t)n streamCap:(int64_t)streamCap {
    if ((self = [super init])) {
        cap = n;
        frames = calloc((size_t)n, sizeof(vtb_frame));
        released = calloc((size_t)n, sizeof(_Atomic uint8_t));
        for (int32_t i = 0; i < n; i++) frames[i].stream_off = -1;
        lock = OS_UNFAIR_LOCK_INIT;
        streamCapBytes = streamCap;
        info = [NSMutableDictionary dictionary];
        setLog = [NSMutableArray array];
        info[@"set"] = setLog;
    }
    return self;
}
- (void)teardown {
    if (session) {
        VTCompressionSessionInvalidate(session);
        CFRelease(session);
        session = NULL;
    }
}
- (void)dealloc {
    [self teardown];
    free(frames);
    free((void *)released);
    free(stream);
    free(scratch);
}
@end

@interface VTBBench : NSObject {
  @public
    int32_t width, height, fmt;
    OSType pf;
    CVPixelBufferPoolRef pool;
    CVPixelBufferRef *ring;
    int32_t ringLen;
    double prerenderMs;
    NSMutableArray<VTBLayer *> *layers;
}
@end

@implementation VTBBench
- (void)dealloc {
    for (int32_t i = 0; i < ringLen; i++)
        if (ring[i]) CVPixelBufferRelease(ring[i]);
    free(ring);
    if (pool) CVPixelBufferPoolRelease(pool);
}
@end

// ---------------------------------------------------------------------------------------
// Encoder output

static bool stream_append(VTBLayer *L, const void *p, size_t n) {
    if (L->streamLen + (int64_t)n > L->streamCapBytes) return false;
    if (L->streamLen + (int64_t)n > L->streamCapacity) {
        int64_t nc = L->streamCapacity ? L->streamCapacity * 2 : (4 << 20);
        while (nc < L->streamLen + (int64_t)n) nc *= 2;
        if (nc > L->streamCapBytes) nc = L->streamCapBytes;
        uint8_t *ns = realloc(L->stream, (size_t)nc);
        if (!ns) return false;
        L->stream = ns;
        L->streamCapacity = nc;
    }
    memcpy(L->stream + L->streamLen, p, n);
    L->streamLen += (int64_t)n;
    return true;
}

static void release_slot(VTBLayer *L, int32_t idx) {
    if (!L->inflight) return;
    uint8_t expected = 0;
    if (atomic_compare_exchange_strong(&L->released[idx], &expected, 1)) dispatch_semaphore_signal(L->inflight);
}

static void enc_output(void *outputRefcon, void *sourceFrameRefcon, OSStatus status, VTEncodeInfoFlags infoFlags,
                       CMSampleBufferRef sb) {
    uint64_t now = mach_absolute_time();
    VTBLayer *L = (__bridge VTBLayer *)outputRefcon;
    int32_t idx = (int32_t)(intptr_t)sourceFrameRefcon;
    atomic_fetch_add(&L->callbacks, 1);
    if (idx < 0 || idx >= L->cap) return;
    vtb_frame *f = &L->frames[idx];
    f->done_ns = ticks_to_ns(now - L->t0);
    f->cflags |= VTB_F_CALLBACK;
    f->cb_status = status;
    if (infoFlags & kVTEncodeInfo_FrameDropped) f->cflags |= VTB_F_DROPPED;
    if (!sb || status != noErr || !CMSampleBufferDataIsReady(sb)) {
        f->cflags |= VTB_F_NO_OUTPUT;
        release_slot(L, idx);
        return;
    }

    bool key = true;
    CFArrayRef atts = CMSampleBufferGetSampleAttachmentsArray(sb, false);
    if (atts && CFArrayGetCount(atts) > 0) {
        CFDictionaryRef d = CFArrayGetValueAtIndex(atts, 0);
        CFBooleanRef notSync = CFDictionaryGetValue(d, kCMSampleAttachmentKey_NotSync);
        if (notSync && CFBooleanGetValue(notSync)) key = false;
    }
    if (key) f->cflags |= VTB_F_KEYFRAME;

    CMBlockBufferRef bb = CMSampleBufferGetDataBuffer(sb);
    size_t total = bb ? CMBlockBufferGetDataLength(bb) : 0;
    f->bytes = (int32_t)total;
    if (L->streamCapBytes <= 0 || total == 0) {
        release_slot(L, idx);
        return;
    }
    char *ptr = NULL;
    size_t lenAt = 0;
    OSStatus s = CMBlockBufferGetDataPointer(bb, 0, &lenAt, NULL, &ptr);
    if (s != kCMBlockBufferNoErr || lenAt < total) {
        if (L->scratchCap < total) {
            free(L->scratch);
            L->scratch = malloc(total);
            L->scratchCap = total;
        }
        CMBlockBufferCopyDataBytes(bb, 0, total, L->scratch);
        ptr = (char *)L->scratch;
    }

    CMFormatDescriptionRef fd = CMSampleBufferGetFormatDescription(sb);
    size_t psCount = 0;
    int nalLen = 4;
    CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fd, 0, NULL, NULL, &psCount, &nalLen);
    static const uint8_t sc[4] = {0, 0, 0, 1};

    os_unfair_lock_lock(&L->lock);
    int64_t off = L->streamLen;
    bool ok = true;
    if (key) {
        for (size_t i = 0; i < psCount && ok; i++) {
            const uint8_t *ps = NULL;
            size_t psLen = 0;
            if (CMVideoFormatDescriptionGetH264ParameterSetAtIndex(fd, i, &ps, &psLen, NULL, NULL) == noErr) {
                ok = stream_append(L, sc, 4) && stream_append(L, ps, psLen);
            }
        }
    }
    size_t p = 0;
    while (ok && p + (size_t)nalLen <= total) {
        uint32_t n = 0;
        for (int k = 0; k < nalLen; k++) n = (n << 8) | (uint8_t)ptr[p + (size_t)k];
        p += (size_t)nalLen;
        if (n == 0 || p + n > total) break;
        ok = stream_append(L, sc, 4) && stream_append(L, ptr + p, n);
        p += n;
    }
    if (ok) {
        f->stream_off = off;
        f->stream_len = (int32_t)(L->streamLen - off);
    } else {
        L->streamLen = off;
        f->cflags |= VTB_F_TRUNCATED;
    }
    os_unfair_lock_unlock(&L->lock);
    release_slot(L, idx);
}

// ---------------------------------------------------------------------------------------
// Session setup

static void set_prop(VTBLayer *L, NSString *name, CFStringRef key, CFTypeRef value) {
    OSStatus st = VTSessionSetProperty(L->session, key, value);
    [L->setLog addObject:@{@"name" : name, @"value" : json_value(value), @"status" : @(st)}];
}

static void readback(VTBLayer *L) {
    if (!L->session) return;
    struct {
        NSString *name;
        CFStringRef key;
    } keys[] = {
        {@"EncoderID", kVTCompressionPropertyKey_EncoderID},
        {@"UsingHardwareAcceleratedVideoEncoder", kVTCompressionPropertyKey_UsingHardwareAcceleratedVideoEncoder},
        {@"EnableLowLatencyRateControl", kVTVideoEncoderSpecification_EnableLowLatencyRateControl},
        {@"ProfileLevel", kVTCompressionPropertyKey_ProfileLevel},
        {@"RealTime", kVTCompressionPropertyKey_RealTime},
        {@"AllowFrameReordering", kVTCompressionPropertyKey_AllowFrameReordering},
        {@"AllowTemporalCompression", kVTCompressionPropertyKey_AllowTemporalCompression},
        {@"ExpectedFrameRate", kVTCompressionPropertyKey_ExpectedFrameRate},
        {@"AverageBitRate", kVTCompressionPropertyKey_AverageBitRate},
        {@"DataRateLimits", kVTCompressionPropertyKey_DataRateLimits},
        {@"MaxKeyFrameInterval", kVTCompressionPropertyKey_MaxKeyFrameInterval},
        {@"MaxKeyFrameIntervalDuration", kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration},
        {@"PrioritizeEncodingSpeedOverQuality", kVTCompressionPropertyKey_PrioritizeEncodingSpeedOverQuality},
        {@"H264EntropyMode", kVTCompressionPropertyKey_H264EntropyMode},
        {@"MaxFrameDelayCount", kVTCompressionPropertyKey_MaxFrameDelayCount},
        {@"ReferenceBufferCount", kVTCompressionPropertyKey_ReferenceBufferCount},
        {@"BaseLayerFrameRateFraction", kVTCompressionPropertyKey_BaseLayerFrameRateFraction},
        {@"MaximizePowerEfficiency", kVTCompressionPropertyKey_MaximizePowerEfficiency},
        {@"PixelBufferPoolIsShared", kVTCompressionPropertyKey_PixelBufferPoolIsShared},
        {@"ColorPrimaries", kVTCompressionPropertyKey_ColorPrimaries},
        {@"YCbCrMatrix", kVTCompressionPropertyKey_YCbCrMatrix},
    };
    NSMutableDictionary *rb = [NSMutableDictionary dictionary];
    NSMutableDictionary *rbErr = [NSMutableDictionary dictionary];
    for (size_t i = 0; i < sizeof(keys) / sizeof(keys[0]); i++) {
        CFTypeRef v = NULL;
        OSStatus st = VTSessionCopyProperty(L->session, keys[i].key, kCFAllocatorDefault, &v);
        if (st == noErr) {
            rb[keys[i].name] = json_value(v);
        } else {
            rbErr[keys[i].name] = @(st);
        }
        if (v) CFRelease(v);
    }
    L->info[@"readback"] = rb;
    L->info[@"readback_status"] = rbErr;

    // What the encoder wants as input: tells whether BGRA needs a conversion.
    CFTypeRef attrs = NULL;
    if (VTSessionCopyProperty(L->session, kVTCompressionPropertyKey_VideoEncoderPixelBufferAttributes,
                              kCFAllocatorDefault, &attrs) == noErr &&
        attrs) {
        NSDictionary *d = (__bridge NSDictionary *)attrs;
        id pfv = d[(id)kCVPixelBufferPixelFormatTypeKey];
        NSMutableArray *names = [NSMutableArray array];
        if ([pfv isKindOfClass:[NSNumber class]]) {
            [names addObject:fourcc([pfv unsignedIntValue])];
        } else if ([pfv isKindOfClass:[NSArray class]]) {
            for (id n in pfv) [names addObject:fourcc([n unsignedIntValue])];
        }
        L->info[@"encoder_pixel_formats"] = names;
        CFRelease(attrs);
    }

    CFDictionaryRef sup = NULL;
    if (VTSessionCopySupportedPropertyDictionary(L->session, &sup) == noErr && sup) {
        NSArray *k = [[(__bridge NSDictionary *)sup allKeys] sortedArrayUsingSelector:@selector(compare:)];
        L->info[@"supported_properties"] = k;
        CFRelease(sup);
    }
}

static OSStatus create_session(VTBLayer *L, int w, int h, OSType inputPF) {
    vtb_enc_config *c = &L->cfg;
    L->inputFormat = inputPF;
    NSMutableDictionary *spec = [NSMutableDictionary dictionary];
    if (c->software) {
        spec[(__bridge id)kVTVideoEncoderSpecification_EnableHardwareAcceleratedVideoEncoder] = @NO;
    } else if (c->require_hw) {
        spec[(__bridge id)kVTVideoEncoderSpecification_RequireHardwareAcceleratedVideoEncoder] = @YES;
    }
    if (c->low_latency_rc) spec[(__bridge id)kVTVideoEncoderSpecification_EnableLowLatencyRateControl] = @YES;
    L->info[@"width"] = @(w);
    L->info[@"height"] = @(h);
    L->info[@"input_format"] = fourcc(inputPF);
    L->info[@"spec"] = json_value((__bridge CFTypeRef)spec);

    uint64_t c0 = mach_absolute_time();
    OSStatus st = VTCompressionSessionCreate(kCFAllocatorDefault, w, h, kCMVideoCodecType_H264,
                                             (__bridge CFDictionaryRef)spec,
                                             (__bridge CFDictionaryRef)pb_attrs(w, h, inputPF), kCFAllocatorDefault,
                                             enc_output, (__bridge void *)L, &L->session);
    L->info[@"create_ms"] = @(ms_since(c0));
    L->info[@"create_status"] = @(st);
    if (st != noErr) {
        L->session = NULL;
        return st;
    }

    set_prop(L, @"ProfileLevel", kVTCompressionPropertyKey_ProfileLevel, kVTProfileLevel_H264_High_AutoLevel);
    set_prop(L, @"RealTime", kVTCompressionPropertyKey_RealTime, c->real_time ? kCFBooleanTrue : kCFBooleanFalse);
    set_prop(L, @"AllowFrameReordering", kVTCompressionPropertyKey_AllowFrameReordering, kCFBooleanFalse);
    if (c->expected_fps > 0) {
        set_prop(L, @"ExpectedFrameRate", kVTCompressionPropertyKey_ExpectedFrameRate,
                 (__bridge CFTypeRef) @(c->expected_fps));
    }
    set_prop(L, @"AverageBitRate", kVTCompressionPropertyKey_AverageBitRate, (__bridge CFTypeRef) @(c->bitrate));
    if (c->data_rate_factor > 0) {
        double bytes = (double)c->bitrate * c->data_rate_factor / 8.0;
        NSArray *lim = @[ @((long long)bytes), @1.0 ];
        set_prop(L, @"DataRateLimits", kVTCompressionPropertyKey_DataRateLimits, (__bridge CFTypeRef)lim);
    }
    if (c->keyint_sec > 0) {
        set_prop(L, @"MaxKeyFrameInterval", kVTCompressionPropertyKey_MaxKeyFrameInterval,
                 (__bridge CFTypeRef) @((int)lround(c->keyint_sec * c->fps)));
        set_prop(L, @"MaxKeyFrameIntervalDuration", kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration,
                 (__bridge CFTypeRef) @(c->keyint_sec));
    }
    if (c->prioritize_speed) {
        set_prop(L, @"PrioritizeEncodingSpeedOverQuality", kVTCompressionPropertyKey_PrioritizeEncodingSpeedOverQuality,
                 kCFBooleanTrue);
    }
    if (c->software) {
        // Apple's software encoder otherwise buffers many frames before output.
        set_prop(L, @"MaxFrameDelayCount", kVTCompressionPropertyKey_MaxFrameDelayCount, (__bridge CFTypeRef) @0);
    }
    set_prop(L, @"ColorPrimaries", kVTCompressionPropertyKey_ColorPrimaries,
             kCMFormatDescriptionColorPrimaries_ITU_R_709_2);
    set_prop(L, @"TransferFunction", kVTCompressionPropertyKey_TransferFunction,
             kCMFormatDescriptionTransferFunction_ITU_R_709_2);
    set_prop(L, @"YCbCrMatrix", kVTCompressionPropertyKey_YCbCrMatrix, kCMFormatDescriptionYCbCrMatrix_ITU_R_709_2);

    c0 = mach_absolute_time();
    OSStatus pst = VTCompressionSessionPrepareToEncodeFrames(L->session);
    L->info[@"prepare_ms"] = @(ms_since(c0));
    L->info[@"prepare_status"] = @(pst);
    return noErr;
}

// ---------------------------------------------------------------------------------------
// Public API

vtb_bench *vtb_open(int32_t width, int32_t height, int32_t fmt, int32_t ring_max, int64_t ring_bytes_max, char *err,
                    int32_t errlen) {
    tb_init();
    glyphs_init();
    @autoreleasepool {
        VTBBench *b = [VTBBench new];
        b->width = width;
        b->height = height;
        b->fmt = fmt;
        b->pf = fmt == VTB_FMT_NV12 ? kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange : kCVPixelFormatType_32BGRA;
        b->layers = [NSMutableArray array];
        uint64_t t0 = mach_absolute_time();

        size_t frameBytes = fmt == VTB_FMT_NV12 ? (size_t)width * height * 3 / 2 : (size_t)width * height * 4;
        int32_t n = ring_max;
        if (ring_bytes_max > 0 && (int64_t)frameBytes * n > ring_bytes_max) n = (int32_t)(ring_bytes_max / (int64_t)frameBytes);
        if (n < 8) n = 8;

        NSDictionary *poolAttrs = @{(id)kCVPixelBufferPoolMinimumBufferCountKey : @(n)};
        CVReturn cr = CVPixelBufferPoolCreate(kCFAllocatorDefault, (__bridge CFDictionaryRef)poolAttrs,
                                              (__bridge CFDictionaryRef)pb_attrs(width, height, b->pf), &b->pool);
        if (cr != kCVReturnSuccess) {
            snprintf(err, (size_t)errlen, "CVPixelBufferPoolCreate: %d", cr);
            return NULL;
        }
        b->ring = calloc((size_t)n, sizeof(CVPixelBufferRef));
        for (int32_t i = 0; i < n; i++) {
            cr = CVPixelBufferPoolCreatePixelBuffer(kCFAllocatorDefault, b->pool, &b->ring[i]);
            if (cr != kCVReturnSuccess) {
                snprintf(err, (size_t)errlen, "CVPixelBufferPoolCreatePixelBuffer #%d: %d", i, cr);
                b->ringLen = i;
                return NULL;
            }
            b->ringLen = i + 1;
        }

        layout_t lay = make_layout(width, height);
        CVPixelBufferRef scratch = NULL;
        VTPixelTransferSessionRef xfer = NULL;
        if (fmt == VTB_FMT_NV12) {
            cr = CVPixelBufferCreate(kCFAllocatorDefault, (size_t)width, (size_t)height, kCVPixelFormatType_32BGRA,
                                     (__bridge CFDictionaryRef)pb_attrs(width, height, kCVPixelFormatType_32BGRA),
                                     &scratch);
            OSStatus st = VTPixelTransferSessionCreate(kCFAllocatorDefault, &xfer);
            if (cr != kCVReturnSuccess || st != noErr) {
                snprintf(err, (size_t)errlen, "scratch buffer %d / VTPixelTransferSessionCreate %d", cr, (int)st);
                if (scratch) CVPixelBufferRelease(scratch);
                if (xfer) CFRelease(xfer);
                return NULL;
            }
            xfer_set_709(xfer);
            set_color_attachments(scratch);
        }
        for (int32_t i = 0; i < n; i++) {
            if (fmt == VTB_FMT_NV12) {
                render_frame(scratch, &lay, i);
                OSStatus st = VTPixelTransferSessionTransferImage(xfer, scratch, b->ring[i]);
                if (st != noErr) {
                    snprintf(err, (size_t)errlen, "VTPixelTransferSessionTransferImage (BGRA->NV12): %d", (int)st);
                    CVPixelBufferRelease(scratch);
                    VTPixelTransferSessionInvalidate(xfer);
                    CFRelease(xfer);
                    return NULL;
                }
            } else {
                render_frame(b->ring[i], &lay, i);
            }
            set_color_attachments(b->ring[i]);
        }
        if (xfer) {
            VTPixelTransferSessionInvalidate(xfer);
            CFRelease(xfer);
        }
        if (scratch) CVPixelBufferRelease(scratch);
        b->prerenderMs = ms_since(t0);
        return (__bridge_retained void *)b;
    }
}

int32_t vtb_ring_len(vtb_bench *bp) { return ((__bridge VTBBench *)bp)->ringLen; }
double vtb_prerender_ms(vtb_bench *bp) { return ((__bridge VTBBench *)bp)->prerenderMs; }

static void *thread_main(void *arg) {
    @autoreleasepool {
        void (^work)(void) = (__bridge_transfer id)arg;
        work();
    }
    return NULL;
}

static void run_on_rt_thread(void (^work)(void)) {
    pthread_attr_t attr;
    pthread_attr_init(&attr);
    pthread_attr_set_qos_class_np(&attr, QOS_CLASS_USER_INTERACTIVE, 0);
    pthread_t th;
    void *arg = (__bridge_retained void *)[work copy];
    if (pthread_create(&th, &attr, thread_main, arg) != 0) {
        thread_main(arg);
    } else {
        pthread_join(th, NULL);
    }
    pthread_attr_destroy(&attr);
}

int32_t vtb_run(vtb_bench *bp, const vtb_enc_config *fullCfg, const vtb_run_config *runCfg) {
    tb_init();
    @autoreleasepool {
        VTBBench *b = (__bridge VTBBench *)bp;
        [b->layers removeAllObjects];
        const vtb_run_config run = *runCfg;

        VTBLayer *full = [[VTBLayer alloc] initWithCapacity:run.frames streamCap:run.stream_cap_bytes];
        full->cfg = *fullCfg;
        full->info[@"role"] = @"full";
        [b->layers addObject:full];
        VTBLayer *prev = nil;
        int ratio = 1;
        if (run.preview) {
            ratio = run.preview_cfg.fps > 0 ? fullCfg->fps / run.preview_cfg.fps : 4;
            if (ratio < 1) ratio = 1;
            prev = [[VTBLayer alloc] initWithCapacity:run.frames / ratio + 2 streamCap:run.stream_cap_bytes];
            prev->cfg = run.preview_cfg;
            prev->info[@"role"] = @"preview";
            [b->layers addObject:prev];
        }

        if (run.encode_full) {
            OSStatus st = create_session(full, b->width, b->height, b->pf);
            if (st != noErr) return st;
        } else {
            full->info[@"skipped"] = @YES;
        }
        VTPixelTransferSessionRef xfer = NULL;
        CVPixelBufferPoolRef prevPool = NULL;
        if (prev) {
            OSStatus st = create_session(prev, run.preview_cfg.width, run.preview_cfg.height,
                                         kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange);
            if (st != noErr) {
                [full teardown];
                return st;
            }
            st = VTPixelTransferSessionCreate(kCFAllocatorDefault, &xfer);
            prev->info[@"xfer_create_status"] = @(st);
            if (st != noErr) {
                [full teardown];
                [prev teardown];
                return st;
            }
            prev->info[@"xfer_realtime_status"] =
                @(VTSessionSetProperty(xfer, kVTPixelTransferPropertyKey_RealTime, kCFBooleanTrue));
            xfer_set_709(xfer);
            NSDictionary *pa = @{(id)kCVPixelBufferPoolMinimumBufferCountKey : @6};
            CVPixelBufferPoolCreate(
                kCFAllocatorDefault, (__bridge CFDictionaryRef)pa,
                (__bridge CFDictionaryRef)pb_attrs(run.preview_cfg.width, run.preview_cfg.height,
                                                   kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange),
                &prevPool);
        }
        dispatch_queue_t prevQueue = nil;
        if (prev) {
            prevQueue = dispatch_queue_create(
                "vtbench.preview", dispatch_queue_attr_make_with_qos_class(DISPATCH_QUEUE_SERIAL, QOS_CLASS_USER_INTERACTIVE, 0));
        }
        if (run.mode == VTB_MODE_THROUGHPUT) {
            full->inflight = dispatch_semaphore_create(run.inflight > 0 ? run.inflight : 4);
        }

        CVPixelBufferRef *ring = b->ring;
        const int32_t ringLen = b->ringLen;
        const int fps = fullCfg->fps;
        NSDictionary *forceKF = @{(__bridge id)kVTEncodeFrameOptionKey_ForceKeyFrame : @YES};
        __block int32_t submitted = 0, prevSubmitted = 0;
        __block int64_t loopNs = 0, endNs = 0;

        struct rusage ru0, ru1;
        getrusage(RUSAGE_SELF, &ru0);
        __block kern_return_t rtStatus = KERN_SUCCESS;
        run_on_rt_thread(^{
          if (run.mode == VTB_MODE_PACED) {
              // Real-time (time-constraint) policy so mach_wait_until wakes on time, like a
              // display-link / SCK delivery thread. Budget: 3 ms of CPU per frame interval.
              int64_t period = (int64_t)(1e9 / fps);
              thread_time_constraint_policy_data_t pol = {
                  .period = (uint32_t)ns_to_ticks(period),
                  .computation = (uint32_t)ns_to_ticks(3 * 1000 * 1000),
                  .constraint = (uint32_t)ns_to_ticks(period / 2),
                  .preemptible = 1,
              };
              rtStatus = thread_policy_set(mach_thread_self(), THREAD_TIME_CONSTRAINT_POLICY, (thread_policy_t)&pol,
                                           THREAD_TIME_CONSTRAINT_POLICY_COUNT);
          }
          uint64_t start = mach_absolute_time() + ns_to_ticks(20 * 1000 * 1000);
          full->t0 = start;
          if (prev) prev->t0 = start;
          mach_wait_until(start);
          int32_t j = 0;
          for (int32_t i = 0; i < run.frames; i++) {
              int64_t target = (int64_t)((double)i * 1e9 / fps);
              if (run.mode == VTB_MODE_PACED) {
                  mach_wait_until(start + ns_to_ticks(target));
              } else {
                  if (ticks_to_ns(mach_absolute_time() - start) > (int64_t)(run.max_seconds * 1e9)) break;
                  dispatch_semaphore_wait(full->inflight, DISPATCH_TIME_FOREVER);
              }
              CVPixelBufferRef pb = ring[i % ringLen];
              if (prev && (i % ratio) == 0 && j < prev->cap) {
                  // The preview path (downscale + encode) runs on its own serial queue, like a
                  // per-layer queue in the engine, so a blocking full-layer EncodeFrame
                  // doesn't delay it and vice versa.
                  const int32_t pj = j;
                  vtb_frame *pf = &prev->frames[pj];
                  pf->frame_index = i;
                  pf->target_ns = target;
                  pf->sflags |= VTB_F_SUBMITTED;
                  dispatch_async(prevQueue, ^{
                    uint64_t x0 = mach_absolute_time();
                    CVPixelBufferRef dst = NULL;
                    CVReturn cr = CVPixelBufferPoolCreatePixelBuffer(kCFAllocatorDefault, prevPool, &dst);
                    OSStatus st = cr == kCVReturnSuccess ? VTPixelTransferSessionTransferImage(xfer, pb, dst) : cr;
                    pf->xfer_ns = ticks_to_ns(mach_absolute_time() - x0);
                    if (st == noErr) {
                        set_color_attachments(dst);
                        VTEncodeInfoFlags info = 0;
                        pf->submit_ns = ticks_to_ns(mach_absolute_time() - start);
                        st = VTCompressionSessionEncodeFrame(prev->session, dst, CMTimeMake(pj, run.preview_cfg.fps),
                                                             CMTimeMake(1, run.preview_cfg.fps), NULL,
                                                             (void *)(intptr_t)pj, &info);
                        pf->return_ns = ticks_to_ns(mach_absolute_time() - start);
                        if (info & kVTEncodeInfo_FrameDropped) pf->sflags |= VTB_F_DROPPED;
                    }
                    if (st != noErr) {
                        pf->submit_status = st;
                        pf->sflags |= VTB_F_SUBMIT_FAILED;
                    }
                    if (dst) CVPixelBufferRelease(dst);
                  });
                  j++;
                  prevSubmitted = j;
              }
              if (run.encode_full) {
                  vtb_frame *f = &full->frames[i];
                  f->frame_index = i;
                  f->target_ns = run.mode == VTB_MODE_PACED ? target : 0;
                  f->sflags |= VTB_F_SUBMITTED;
                  bool force = (i == run.force_kf_frame);
                  if (force) f->sflags |= VTB_F_FORCED_KF;
                  VTEncodeInfoFlags info = 0;
                  f->submit_ns = ticks_to_ns(mach_absolute_time() - start);
                  OSStatus st = VTCompressionSessionEncodeFrame(
                      full->session, pb, CMTimeMake(i, fps), CMTimeMake(1, fps),
                      force ? (__bridge CFDictionaryRef)forceKF : NULL, (void *)(intptr_t)i, &info);
                  f->return_ns = ticks_to_ns(mach_absolute_time() - start);
                  if (info & kVTEncodeInfo_FrameDropped) f->sflags |= VTB_F_DROPPED;
                  if (st != noErr) {
                      f->submit_status = st;
                      f->sflags |= VTB_F_SUBMIT_FAILED;
                      release_slot(full, i);
                  }
                  submitted = i + 1;
              }
          }
          if (prevQueue) dispatch_sync(prevQueue, ^{});
          loopNs = ticks_to_ns(mach_absolute_time() - start);
          if (full->session) VTCompressionSessionCompleteFrames(full->session, kCMTimeInvalid);
          if (prev && prev->session) VTCompressionSessionCompleteFrames(prev->session, kCMTimeInvalid);
          endNs = ticks_to_ns(mach_absolute_time() - start);
        });

        getrusage(RUSAGE_SELF, &ru1);
        double cpuMs = (double)(ru1.ru_utime.tv_sec - ru0.ru_utime.tv_sec + ru1.ru_stime.tv_sec - ru0.ru_stime.tv_sec) * 1e3 +
                       (double)(ru1.ru_utime.tv_usec - ru0.ru_utime.tv_usec + ru1.ru_stime.tv_usec - ru0.ru_stime.tv_usec) / 1e3;
        full->info[@"process_cpu_ms"] = @(cpuMs);
        full->info[@"pacing_thread_rt_status"] = @(rtStatus);
        full->used = submitted;
        full->info[@"submitted"] = @(submitted);
        full->info[@"callbacks"] = @(atomic_load(&full->callbacks));
        full->info[@"loop_ns"] = @(loopNs);
        full->info[@"end_ns"] = @(endNs);
        readback(full);
        [full teardown];
        if (prev) {
            prev->used = prevSubmitted;
            prev->info[@"submitted"] = @(prevSubmitted);
            prev->info[@"callbacks"] = @(atomic_load(&prev->callbacks));
            readback(prev);
            [prev teardown];
        }
        if (xfer) {
            VTPixelTransferSessionInvalidate(xfer);
            CFRelease(xfer);
        }
        if (prevPool) CVPixelBufferPoolRelease(prevPool);
        return 0;
    }
}

int32_t vtb_layer_count(vtb_bench *bp) { return (int32_t)((__bridge VTBBench *)bp)->layers.count; }

static VTBLayer *layer_at(vtb_bench *bp, int32_t i) {
    VTBBench *b = (__bridge VTBBench *)bp;
    if (i < 0 || i >= (int32_t)b->layers.count) return nil;
    return b->layers[(NSUInteger)i];
}

char *vtb_layer_info_json(vtb_bench *bp, int32_t layer) {
    @autoreleasepool {
        VTBLayer *L = layer_at(bp, layer);
        if (!L) return strdup("{}");
        return json_dup(L->info);
    }
}

int32_t vtb_layer_frames(vtb_bench *bp, int32_t layer, const vtb_frame **out) {
    VTBLayer *L = layer_at(bp, layer);
    if (!L) return 0;
    *out = L->frames;
    return L->used;
}

int64_t vtb_layer_stream(vtb_bench *bp, int32_t layer, const uint8_t **out) {
    VTBLayer *L = layer_at(bp, layer);
    if (!L) return 0;
    *out = L->stream;
    return L->streamLen;
}

void vtb_close(vtb_bench *bp) {
    if (bp) CFBridgingRelease(bp);
}

void vtb_free(void *p) { free(p); }

// ---------------------------------------------------------------------------------------
// Decode check

static double psnr_y(CVPixelBufferRef a, CVPixelBufferRef b) {
    CVPixelBufferLockBaseAddress(a, kCVPixelBufferLock_ReadOnly);
    CVPixelBufferLockBaseAddress(b, kCVPixelBufferLock_ReadOnly);
    const uint8_t *pa = CVPixelBufferGetBaseAddressOfPlane(a, 0), *pb = CVPixelBufferGetBaseAddressOfPlane(b, 0);
    size_t sa = CVPixelBufferGetBytesPerRowOfPlane(a, 0), sb = CVPixelBufferGetBytesPerRowOfPlane(b, 0);
    size_t w = CVPixelBufferGetWidthOfPlane(a, 0), h = CVPixelBufferGetHeightOfPlane(a, 0);
    if (CVPixelBufferGetWidthOfPlane(b, 0) < w) w = CVPixelBufferGetWidthOfPlane(b, 0);
    if (CVPixelBufferGetHeightOfPlane(b, 0) < h) h = CVPixelBufferGetHeightOfPlane(b, 0);
    enum { BANDS = 32 };
    __block uint64_t acc[BANDS];
    memset(acc, 0, sizeof(acc));
    uint64_t *accp = acc;
    dispatch_apply(BANDS, DISPATCH_APPLY_AUTO, ^(size_t band) {
      uint64_t s = 0;
      for (size_t y = band * h / BANDS; y < (band + 1) * h / BANDS; y++) {
          const uint8_t *ra = pa + y * sa, *rb = pb + y * sb;
          for (size_t x = 0; x < w; x++) {
              int d = (int)ra[x] - (int)rb[x];
              s += (uint64_t)(d * d);
          }
      }
      accp[band] = s;
    });
    uint64_t sse = 0;
    for (int i = 0; i < BANDS; i++) sse += acc[i];
    CVPixelBufferUnlockBaseAddress(b, kCVPixelBufferLock_ReadOnly);
    CVPixelBufferUnlockBaseAddress(a, kCVPixelBufferLock_ReadOnly);
    double mse = (double)sse / (double)(w * h);
    if (mse <= 0) return 99.0;
    return 10.0 * log10(255.0 * 255.0 / mse);
}

int32_t vtb_decode_check(vtb_bench *bp, const uint8_t *annexb, const int64_t *au_off, const int32_t *au_len,
                         const int32_t *au_frame, int32_t n, int32_t psnr, vtb_decode_result *out) {
    tb_init();
    @autoreleasepool {
        VTBBench *b = (__bridge VTBBench *)bp;
        memset(out, 0, sizeof(*out));
        const bool doPSNR = psnr && b->fmt == VTB_FMT_NV12;
        uint64_t t0 = mach_absolute_time();
        __block vtb_decode_result r;
        memset(&r, 0, sizeof(r));
        r.psnr_y_min = 1e9;
        __block double psnrSum = 0;
        __block int32_t inDecode = -1;
        CMVideoFormatDescriptionRef fd = NULL;
        VTDecompressionSessionRef ds = NULL;
        NSData *curSPS = nil, *curPPS = nil;
        NSMutableData *avcc = [NSMutableData data];
        NSDictionary *dst = doPSNR ? pb_attrs(b->width, b->height, kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange)
                                   : @{(id)kCVPixelBufferIOSurfacePropertiesKey : @{}};

        for (int32_t i = 0; i < n; i++) {
            const uint8_t *au = annexb + au_off[i];
            size_t len = (size_t)au_len[i];
            [avcc setLength:0];
            NSData *sps = nil, *pps = nil;
            // split Annex B (3- or 4-byte start codes)
            size_t p = 0, nalStart = SIZE_MAX;
            while (p <= len) {
                bool sc = p + 3 <= len && au[p] == 0 && au[p + 1] == 0 && au[p + 2] == 1;
                if (sc || p == len) {
                    if (nalStart != SIZE_MAX) {
                        size_t e = p;
                        while (e > nalStart && au[e - 1] == 0) e--; // trailing zero of a 4-byte start code
                        if (e > nalStart) {
                            const uint8_t *nal = au + nalStart;
                            size_t nl = e - nalStart;
                            int type = nal[0] & 0x1f;
                            if (type == 7) {
                                sps = [NSData dataWithBytes:nal length:nl];
                            } else if (type == 8) {
                                pps = [NSData dataWithBytes:nal length:nl];
                            } else if (type != 9) {
                                uint8_t be[4] = {(uint8_t)(nl >> 24), (uint8_t)(nl >> 16), (uint8_t)(nl >> 8),
                                                 (uint8_t)nl};
                                [avcc appendBytes:be length:4];
                                [avcc appendBytes:nal length:nl];
                            }
                        }
                    }
                    if (p == len) break;
                    p += 3;
                    nalStart = p;
                    continue;
                }
                p++;
            }
            if ((sps && ![sps isEqualToData:curSPS]) || (pps && ![pps isEqualToData:curPPS])) {
                if (sps) curSPS = sps;
                if (pps) curPPS = pps;
                if (curSPS && curPPS) {
                    const uint8_t *sets[2] = {curSPS.bytes, curPPS.bytes};
                    size_t sizes[2] = {curSPS.length, curPPS.length};
                    CMVideoFormatDescriptionRef nfd = NULL;
                    OSStatus st =
                        CMVideoFormatDescriptionCreateFromH264ParameterSets(kCFAllocatorDefault, 2, sets, sizes, 4, &nfd);
                    if (st != noErr) {
                        r.failed++;
                        if (!r.first_error) r.first_error = st;
                        continue;
                    }
                    r.format_changes++;
                    if (fd) CFRelease(fd);
                    fd = nfd;
                    if (ds && !VTDecompressionSessionCanAcceptFormatDescription(ds, fd)) {
                        VTDecompressionSessionWaitForAsynchronousFrames(ds);
                        VTDecompressionSessionInvalidate(ds);
                        CFRelease(ds);
                        ds = NULL;
                    }
                }
            }
            if (!fd || avcc.length == 0) continue;
            if (!ds) {
                OSStatus st = VTDecompressionSessionCreate(kCFAllocatorDefault, fd, NULL,
                                                           (__bridge CFDictionaryRef)dst, NULL, &ds);
                if (st != noErr) {
                    r.failed++;
                    if (!r.first_error) r.first_error = st;
                    ds = NULL;
                    continue;
                }
            }
            CMBlockBufferRef bb = NULL;
            OSStatus st = CMBlockBufferCreateWithMemoryBlock(kCFAllocatorDefault, NULL, avcc.length, kCFAllocatorDefault,
                                                             NULL, 0, avcc.length, kCMBlockBufferAssureMemoryNowFlag, &bb);
            if (st == noErr) st = CMBlockBufferReplaceDataBytes(avcc.bytes, bb, 0, avcc.length);
            CMSampleBufferRef sb = NULL;
            size_t ssize = avcc.length;
            CMSampleTimingInfo timing = {CMTimeMake(1, 60), CMTimeMake(i, 60), kCMTimeInvalid};
            if (st == noErr) st = CMSampleBufferCreateReady(kCFAllocatorDefault, bb, fd, 1, 1, &timing, 1, &ssize, &sb);
            if (st != noErr) {
                r.failed++;
                if (!r.first_error) r.first_error = st;
                if (bb) CFRelease(bb);
                continue;
            }
            const int32_t idx = i;
            const int32_t srcFrame = au_frame ? au_frame[i] : i;
            inDecode = i;
            VTDecodeInfoFlags infoOut = 0;
            st = VTDecompressionSessionDecodeFrameWithOutputHandler(
                ds, sb, 0, &infoOut, ^(OSStatus s, VTDecodeInfoFlags fl, CVImageBufferRef img, CMTime pts, CMTime dur) {
                  if (s != noErr || !img) {
                      r.failed++;
                      if (!r.first_error) r.first_error = s ? s : -1;
                      return;
                  }
                  r.decoded++;
                  if (idx != inDecode) r.delayed++;
                  r.width = (int32_t)CVPixelBufferGetWidth(img);
                  r.height = (int32_t)CVPixelBufferGetHeight(img);
                  if (doPSNR) {
                      double v = psnr_y(b->ring[srcFrame % b->ringLen], img);
                      psnrSum += v;
                      r.psnr_frames++;
                      if (v < r.psnr_y_min) r.psnr_y_min = v;
                  }
                });
            inDecode = -1;
            if (st != noErr) {
                r.failed++;
                if (!r.first_error) r.first_error = st;
            }
            CFRelease(sb);
            CFRelease(bb);
        }
        if (ds) {
            VTDecompressionSessionFinishDelayedFrames(ds);
            VTDecompressionSessionWaitForAsynchronousFrames(ds);
            VTDecompressionSessionInvalidate(ds);
            CFRelease(ds);
        }
        if (fd) CFRelease(fd);
        if (r.psnr_frames > 0) {
            r.psnr_y_mean = psnrSum / r.psnr_frames;
        } else {
            r.psnr_y_min = 0;
        }
        r.decode_ms = ms_since(t0);
        *out = r;
        return 0;
    }
}

// ---------------------------------------------------------------------------------------
// Encoder list and system info

void *vtb_begin_activity(int32_t latency_critical) {
    NSActivityOptions opts = NSActivityUserInitiated | NSActivityIdleSystemSleepDisabled;
    if (latency_critical) opts |= NSActivityLatencyCritical;
    id tok = [[NSProcessInfo processInfo] beginActivityWithOptions:opts reason:@"vtbench encode benchmark"];
    return (__bridge_retained void *)tok;
}

void vtb_end_activity(void *token) {
    if (!token) return;
    id tok = (__bridge_transfer id)token;
    [[NSProcessInfo processInfo] endActivity:tok];
}

char *vtb_encoders_json(void) {
    @autoreleasepool {
        NSMutableDictionary *root = [NSMutableDictionary dictionary];
        NSMutableArray *encs = [NSMutableArray array];
        CFArrayRef list = NULL;
        OSStatus st = VTCopyVideoEncoderList(NULL, &list);
        root[@"list_status"] = @(st);
        if (st == noErr && list) {
            for (NSDictionary *d in (__bridge NSArray *)list) {
                NSNumber *ct = d[(__bridge id)kVTVideoEncoderList_CodecType];
                if (ct.unsignedIntValue != kCMVideoCodecType_H264) continue;
                NSMutableDictionary *e = [NSMutableDictionary dictionary];
                e[@"encoder_id"] = d[(__bridge id)kVTVideoEncoderList_EncoderID] ?: @"";
                e[@"name"] = d[(__bridge id)kVTVideoEncoderList_EncoderName] ?: @"";
                e[@"hardware"] = d[(__bridge id)kVTVideoEncoderList_IsHardwareAccelerated] ?: @NO;
                if (d[(__bridge id)kVTVideoEncoderList_PerformanceRating])
                    e[@"performance_rating"] = d[(__bridge id)kVTVideoEncoderList_PerformanceRating];
                if (d[(__bridge id)kVTVideoEncoderList_QualityRating])
                    e[@"quality_rating"] = d[(__bridge id)kVTVideoEncoderList_QualityRating];
                if (d[(__bridge id)kVTVideoEncoderList_InstanceLimit])
                    e[@"instance_limit"] = d[(__bridge id)kVTVideoEncoderList_InstanceLimit];
                if (d[(__bridge id)kVTVideoEncoderList_SupportsFrameReordering])
                    e[@"supports_frame_reordering"] = d[(__bridge id)kVTVideoEncoderList_SupportsFrameReordering];
                NSDictionary *sel = d[(__bridge id)kVTVideoEncoderList_SupportedSelectionProperties];
                if (sel) e[@"selection_properties"] = [[sel allKeys] sortedArrayUsingSelector:@selector(compare:)];
                [encs addObject:e];
            }
            CFRelease(list);
        }
        root[@"h264"] = encs;

        // Which encoder VT would pick per size, with and without low-latency RC.
        NSMutableArray *probes = [NSMutableArray array];
        int sizes[3][2] = {{1920, 1080}, {2560, 1440}, {3840, 2160}};
        for (int s = 0; s < 3; s++) {
            for (int llrc = 0; llrc <= 1; llrc++) {
                NSMutableDictionary *spec = [NSMutableDictionary dictionary];
                spec[(__bridge id)kVTVideoEncoderSpecification_RequireHardwareAcceleratedVideoEncoder] = @YES;
                if (llrc) spec[(__bridge id)kVTVideoEncoderSpecification_EnableLowLatencyRateControl] = @YES;
                CFStringRef encID = NULL;
                CFDictionaryRef props = NULL;
                OSStatus ps = VTCopySupportedPropertyDictionaryForEncoder(
                    sizes[s][0], sizes[s][1], kCMVideoCodecType_H264, (__bridge CFDictionaryRef)spec, &encID, &props);
                NSMutableDictionary *pr = [NSMutableDictionary dictionary];
                pr[@"width"] = @(sizes[s][0]);
                pr[@"height"] = @(sizes[s][1]);
                pr[@"low_latency_rc"] = llrc ? @YES : @NO;
                pr[@"status"] = @(ps);
                if (encID) {
                    pr[@"encoder_id"] = (__bridge NSString *)encID;
                    CFRelease(encID);
                }
                if (props) {
                    pr[@"supported_property_count"] = @(CFDictionaryGetCount(props));
                    CFRelease(props);
                }
                [probes addObject:pr];
            }
        }
        root[@"probes"] = probes;
        return json_dup(root);
    }
}

static NSString *sysctl_str(const char *name) {
    char buf[256];
    size_t len = sizeof(buf);
    if (sysctlbyname(name, buf, &len, NULL, 0) != 0) return @"";
    buf[sizeof(buf) - 1] = 0;
    return [NSString stringWithUTF8String:buf];
}

char *vtb_system_json(void) {
    @autoreleasepool {
        NSProcessInfo *pi = [NSProcessInfo processInfo];
        NSOperatingSystemVersion v = pi.operatingSystemVersion;
        NSMutableDictionary *d = [NSMutableDictionary dictionary];
        d[@"model"] = sysctl_str("hw.model");
        d[@"cpu"] = sysctl_str("machdep.cpu.brand_string");
        d[@"os"] = [NSString stringWithFormat:@"macOS %ld.%ld.%ld", (long)v.majorVersion, (long)v.minorVersion,
                                              (long)v.patchVersion];
        d[@"os_build"] = sysctl_str("kern.osversion");
        uint64_t mem = 0;
        size_t len = sizeof(mem);
        sysctlbyname("hw.memsize", &mem, &len, NULL, 0);
        d[@"memory_gb"] = @(mem / (1024ULL * 1024 * 1024));
        d[@"cores"] = @(pi.activeProcessorCount);
        static NSString *const thermal[] = {@"nominal", @"fair", @"serious", @"critical"};
        NSInteger ts = pi.thermalState;
        d[@"thermal_state"] = (ts >= 0 && ts < 4) ? thermal[ts] : @"unknown";
        d[@"low_power_mode"] = @(pi.lowPowerModeEnabled);
        double la[3] = {0, 0, 0};
        if (getloadavg(la, 3) == 3) {
            NSMutableArray *a = [NSMutableArray array];
            for (int i = 0; i < 3; i++)
                [a addObject:[NSDecimalNumber decimalNumberWithString:[NSString stringWithFormat:@"%.2f", la[i]]]];
            d[@"load_avg"] = a;
        }
        CFTypeRef blob = IOPSCopyPowerSourcesInfo();
        if (blob) {
            CFStringRef src = IOPSGetProvidingPowerSourceType(blob);
            if (src) d[@"power_source"] = (__bridge NSString *)src;
            CFRelease(blob);
        }
        return json_dup(d);
    }
}
