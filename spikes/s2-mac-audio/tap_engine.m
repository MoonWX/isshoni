// tap_engine.m: macOS implementation of the isshoni audio ABI (spike S2).
//
// "All system audio except excluded apps" is one Core Audio process tap
// (CATapDescription initStereoGlobalTapButExcludeProcesses:, macOS 14.2+) on a
// private aggregate device. The exclude list is kept current from the process
// object list (listener + 250 ms poll) and pushed to the live tap by setting
// kAudioTapPropertyDescription. On macOS 26+ the tap also excludes by bundle ID
// (bundleIDs + processRestoreEnabled), which covers helpers that start later.
// Excluding (or re-including) an app while it is audible would cut it
// mid-waveform, so that case builds a second tap and crossfades (see IMGen).
//
// Threads: all engine state lives on one serial queue (q). The HAL IO thread
// only writes an SPSC ring; im_audio_read (one consumer) drains it, resampling
// to 48 kHz with an AudioConverter when the tap runs at another rate.

#import <AudioToolbox/AudioToolbox.h>
#import <CoreAudio/AudioHardwareTapping.h>
#import <CoreAudio/CATapDescription.h>
#import <CoreAudio/CoreAudio.h>
#import <Foundation/Foundation.h>
#include <dlfcn.h>
#include <libproc.h>
#include <mach/mach_time.h>
#include <os/lock.h>
#include <pthread.h>
#include <stdatomic.h>
#include <sys/sysctl.h>
#include <unistd.h>

#include "isshoni_audio.h"

#pragma mark - small helpers

static __thread char tlErr[512];

static void setErr(NSString *msg) { strlcpy(tlErr, msg.UTF8String ?: "", sizeof tlErr); }

static NSString *osstr(OSStatus st) {
  UInt32 be = CFSwapInt32HostToBig((UInt32)st);
  char c[5] = {0};
  memcpy(c, &be, 4);
  for (int i = 0; i < 4; i++)
    if (c[i] < 32 || c[i] > 126) return [NSString stringWithFormat:@"%d", (int)st];
  return [NSString stringWithFormat:@"'%s' (%d)", c, (int)st];
}

static mach_timebase_info_data_t timebase(void) {
  static mach_timebase_info_data_t tb;
  static dispatch_once_t once;
  dispatch_once(&once, ^{ mach_timebase_info(&tb); });
  return tb;
}

static int64_t hostToNs(uint64_t host) {
  mach_timebase_info_data_t tb = timebase();
  return (int64_t)((__uint128_t)host * tb.numer / tb.denom);
}

int64_t im_now_ns(void) { return hostToNs(mach_absolute_time()); }

static AudioObjectPropertyAddress addrOf(AudioObjectPropertySelector sel) {
  return (AudioObjectPropertyAddress){sel, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
}

static OSStatus getU32(AudioObjectID o, AudioObjectPropertySelector sel, UInt32 *v) {
  AudioObjectPropertyAddress a = addrOf(sel);
  UInt32 sz = sizeof *v;
  return AudioObjectGetPropertyData(o, &a, 0, NULL, &sz, v);
}

static NSString *getStr(AudioObjectID o, AudioObjectPropertySelector sel) {
  AudioObjectPropertyAddress a = addrOf(sel);
  CFStringRef s = NULL;
  UInt32 sz = sizeof s;
  if (AudioObjectGetPropertyData(o, &a, 0, NULL, &sz, &s) != noErr || !s) return nil;
  return (__bridge_transfer NSString *)s;
}

static int32_t writeJSON(id obj, char *json, int32_t cap) {
  NSError *e = nil;
  NSData *d = [NSJSONSerialization dataWithJSONObject:obj options:0 error:&e];
  if (!d) {
    setErr([NSString stringWithFormat:@"json: %@", e.localizedDescription]);
    return IM_ERR_INTERNAL;
  }
  if (!json || cap < (int32_t)d.length + 1) return IM_ERR_BUFFER_TOO_SMALL;
  memcpy(json, d.bytes, d.length);
  json[d.length] = 0;
  return (int32_t)d.length;
}

#pragma mark - processes

// responsibility_get_pid_responsible_for_pid is SPI (libquarantine via
// libSystem); TCC uses the same notion. Looked up at runtime.
static pid_t (*respFn)(pid_t);

static void initSPI(void) {
  static dispatch_once_t once;
  dispatch_once(&once, ^{ respFn = (pid_t(*)(pid_t))dlsym(RTLD_DEFAULT, "responsibility_get_pid_responsible_for_pid"); });
}

static BOOL kinfo(pid_t pid, pid_t *ppid, NSString **name) {
  struct kinfo_proc kp;
  size_t len = sizeof kp;
  int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_PID, pid};
  if (sysctl(mib, 4, &kp, &len, NULL, 0) != 0 || len == 0) return NO;
  if (ppid) *ppid = kp.kp_eproc.e_ppid;
  if (name) *name = [NSString stringWithUTF8String:kp.kp_proc.p_comm] ?: @"";
  return YES;
}

static NSString *pathOf(pid_t pid) {
  char buf[PROC_PIDPATHINFO_MAXSIZE];
  int n = proc_pidpath(pid, buf, sizeof buf);
  return n > 0 ? [[NSString alloc] initWithBytes:buf length:(NSUInteger)n encoding:NSUTF8StringEncoding] : nil;
}

// Bundle IDs of every bundle (.app/.xpc/.appex) enclosing path, inner → outer.
static NSMutableDictionary<NSString *, NSArray<NSString *> *> *gBundleCache; // q only

static NSArray<NSString *> *enclosingBundleIDs(NSString *path) {
  if (!path.length) return @[];
  if (!gBundleCache) gBundleCache = [NSMutableDictionary dictionary];
  NSArray *hit = gBundleCache[path];
  if (hit) return hit;
  NSMutableArray *ids = [NSMutableArray array];
  NSString *p = path;
  while (p.length > 1) {
    p = p.stringByDeletingLastPathComponent;
    NSString *ext = p.pathExtension.lowercaseString;
    if ([ext isEqualToString:@"app"] || [ext isEqualToString:@"xpc"] || [ext isEqualToString:@"appex"]) {
      NSDictionary *info = [NSDictionary dictionaryWithContentsOfFile:[p stringByAppendingPathComponent:@"Contents/Info.plist"]];
      NSString *bid = info[@"CFBundleIdentifier"];
      if ([bid isKindOfClass:NSString.class] && bid.length) [ids addObject:bid];
    }
  }
  if (gBundleCache.count > 2000) [gBundleCache removeAllObjects];
  gBundleCache[path] = ids;
  return ids;
}

static BOOL hasAncestor(pid_t pid, pid_t target) {
  for (int i = 0; i < 64 && pid > 1; i++) {
    pid_t pp = -1;
    if (!kinfo(pid, &pp, NULL) || pp == pid) return NO;
    if (pp == target) return YES;
    pid = pp;
  }
  return NO;
}

static BOOL idMatches(NSString *rule, NSString *bid) {
  if (!bid.length || !rule.length) return NO;
  NSString *b = bid.lowercaseString, *r = rule.lowercaseString;
  return [b isEqualToString:r] || [b hasPrefix:[r stringByAppendingString:@"."]];
}

@interface IMProc : NSObject
@property(nonatomic) AudioObjectID obj;
@property(nonatomic) pid_t pid, ppid, rpid;
@property(nonatomic, copy) NSString *name, *bundleID, *path, *respName;
@property(nonatomic, copy) NSArray<NSString *> *appIDs;  // enclosing bundles of the executable
@property(nonatomic, copy) NSArray<NSString *> *respIDs; // enclosing bundles of the responsible process
@property(nonatomic) BOOL output, input, excluded;
@property(nonatomic) BOOL micUse;                             // input on a real (non-aggregate) device
@property(nonatomic, copy) NSArray<NSString *> *inputDevices; // transport types of the devices it records from
@property(nonatomic, copy) NSString *reason, *ruleBundleID;
@end
@implementation IMProc
@end

static NSArray<NSNumber *> *processObjects(void) {
  AudioObjectPropertyAddress a = addrOf(kAudioHardwarePropertyProcessObjectList);
  UInt32 sz = 0;
  if (AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &a, 0, NULL, &sz) != noErr || sz == 0) return @[];
  AudioObjectID *ids = calloc(sz / sizeof(AudioObjectID) + 1, sizeof(AudioObjectID));
  if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &a, 0, NULL, &sz, ids) != noErr) {
    free(ids);
    return @[];
  }
  NSMutableArray *out = [NSMutableArray array];
  for (UInt32 i = 0; i < sz / sizeof(AudioObjectID); i++) [out addObject:@(ids[i])];
  free(ids);
  return out;
}

static NSString *fourcc(UInt32 v) {
  char c[5] = {(char)(v >> 24), (char)(v >> 16), (char)(v >> 8), (char)v, 0};
  for (int i = 0; i < 4; i++)
    if (c[i] < 32 || c[i] > 126) return [NSString stringWithFormat:@"0x%08x", (unsigned)v];
  return [NSString stringWithUTF8String:c];
}

// IsRunningInput is also true for processes that only read a process tap
// (isshoni itself, screen recorders): they record from a private aggregate
// device, which other processes see as an aggregate or cannot resolve at all.
// A microphone user records from a real device (built-in, USB, Bluetooth,
// or a virtual mic such as Krisp).
static void inputDevices(AudioObjectID o, IMProc *p) {
  AudioObjectPropertyAddress a = {kAudioProcessPropertyDevices, kAudioObjectPropertyScopeInput, kAudioObjectPropertyElementMain};
  AudioObjectID ids[32];
  UInt32 sz = sizeof ids;
  NSMutableArray *kinds = [NSMutableArray array];
  BOOL mic = NO;
  if (AudioObjectGetPropertyData(o, &a, 0, NULL, &sz, ids) == noErr) {
    for (UInt32 i = 0; i < sz / sizeof(AudioObjectID); i++) {
      UInt32 tt = 0;
      if (getU32(ids[i], kAudioDevicePropertyTransportType, &tt) != noErr) {
        [kinds addObject:@"unresolvable"];
        continue;
      }
      [kinds addObject:fourcc(tt)];
      if (tt != kAudioDeviceTransportTypeAggregate && tt != kAudioDeviceTransportTypeAutoAggregate) mic = YES;
    }
  }
  p.inputDevices = kinds;
  p.micUse = mic;
}

static NSArray<IMProc *> *snapshot(void) {
  initSPI();
  NSMutableArray *out = [NSMutableArray array];
  for (NSNumber *n in processObjects()) {
    AudioObjectID o = n.unsignedIntValue;
    pid_t pid = -1;
    UInt32 sz = sizeof pid;
    AudioObjectPropertyAddress a = addrOf(kAudioProcessPropertyPID);
    if (AudioObjectGetPropertyData(o, &a, 0, NULL, &sz, &pid) != noErr || pid <= 0) continue;
    IMProc *p = [IMProc new];
    p.obj = o;
    p.pid = pid;
    p.bundleID = getStr(o, kAudioProcessPropertyBundleID) ?: @"";
    UInt32 v = 0;
    if (getU32(o, kAudioProcessPropertyIsRunningOutput, &v) == noErr) p.output = v != 0;
    v = 0;
    if (getU32(o, kAudioProcessPropertyIsRunningInput, &v) == noErr) p.input = v != 0;
    if (p.input) inputDevices(o, p);
    pid_t pp = -1;
    NSString *name = @"";
    kinfo(pid, &pp, &name);
    p.ppid = pp;
    p.name = name;
    p.path = pathOf(pid);
    p.appIDs = enclosingBundleIDs(p.path);
    pid_t r = respFn ? respFn(pid) : pid;
    p.rpid = r > 0 ? r : pid;
    p.respIDs = @[];
    p.respName = @"";
    if (p.rpid != pid) {
      p.respIDs = enclosingBundleIDs(pathOf(p.rpid));
      NSString *rn = @"";
      kinfo(p.rpid, NULL, &rn);
      p.respName = rn;
    }
    [out addObject:p];
  }
  return out;
}

#pragma mark - capture ring (IO thread → reader)

typedef struct {
  float *buf; // interleaved stereo
  uint64_t cap, mask; // frames (power of two)
  _Atomic uint64_t w, r;
  _Atomic uint32_t anchorSeq; // seqlock over anchorHost/anchorFrame
  _Atomic uint64_t anchorHost, anchorFrame;
  _Atomic uint64_t callbacks, frames, zeroRun, overflow;
  _Atomic uint32_t maxPeak, recentPeak; // float bits (non-negative floats order like ints)
  _Atomic int stopped;
  dispatch_semaphore_t sem;
} Ring;

static void peakMax(_Atomic uint32_t *slot, float v) {
  uint32_t bits;
  memcpy(&bits, &v, 4);
  uint32_t cur = atomic_load_explicit(slot, memory_order_relaxed);
  while (bits > cur && !atomic_compare_exchange_weak_explicit(slot, &cur, bits, memory_order_relaxed, memory_order_relaxed)) {
  }
}

static float peakGet(_Atomic uint32_t *slot, BOOL reset) {
  uint32_t bits = reset ? atomic_exchange(slot, 0) : atomic_load(slot);
  float f;
  memcpy(&f, &bits, 4);
  return f;
}

// Real-time: no locks, no allocation, no ObjC.
static void ringWrite(Ring *g, const AudioBufferList *in, const AudioTimeStamp *t, UInt32 ch, bool nonInter) {
  atomic_fetch_add_explicit(&g->callbacks, 1, memory_order_relaxed);
  if (!in || in->mNumberBuffers == 0 || ch == 0) return;
  const float *L, *R;
  UInt32 frames, stride;
  if (nonInter) {
    if (in->mNumberBuffers < ch) return;
    const AudioBuffer *b = &in->mBuffers[in->mNumberBuffers - ch]; // taps come after any device inputs
    frames = b->mDataByteSize / sizeof(float);
    L = b->mData;
    R = ch > 1 ? b[1].mData : L;
    stride = 1;
  } else {
    const AudioBuffer *b = &in->mBuffers[in->mNumberBuffers - 1];
    stride = b->mNumberChannels ? b->mNumberChannels : 1;
    frames = b->mDataByteSize / (sizeof(float) * stride);
    L = b->mData;
    R = stride > 1 ? L + 1 : L;
  }
  if (!L || !R || frames == 0) return;
  uint64_t w = atomic_load_explicit(&g->w, memory_order_relaxed);
  if (t && (t->mFlags & kAudioTimeStampHostTimeValid)) {
    atomic_fetch_add_explicit(&g->anchorSeq, 1, memory_order_acq_rel); // odd: writing
    atomic_store_explicit(&g->anchorHost, t->mHostTime, memory_order_relaxed);
    atomic_store_explicit(&g->anchorFrame, w, memory_order_relaxed);
    atomic_fetch_add_explicit(&g->anchorSeq, 1, memory_order_acq_rel); // even: done
  }
  uint64_t r = atomic_load_explicit(&g->r, memory_order_acquire);
  uint64_t space = g->cap - (w - r);
  if (frames > space) {
    atomic_fetch_add_explicit(&g->overflow, frames - space, memory_order_relaxed);
    frames = (UInt32)space;
  }
  float peak = 0;
  for (UInt32 i = 0; i < frames; i++) {
    float l = L[(size_t)i * stride], rr = R[(size_t)i * stride];
    uint64_t idx = (w + i) & g->mask;
    g->buf[idx * 2] = l;
    g->buf[idx * 2 + 1] = rr;
    float a = fabsf(l) > fabsf(rr) ? fabsf(l) : fabsf(rr);
    if (a > peak) peak = a;
  }
  if (peak == 0)
    atomic_fetch_add_explicit(&g->zeroRun, frames, memory_order_relaxed);
  else
    atomic_store_explicit(&g->zeroRun, 0, memory_order_relaxed);
  peakMax(&g->maxPeak, peak);
  peakMax(&g->recentPeak, peak);
  atomic_store_explicit(&g->w, w + frames, memory_order_release);
  atomic_fetch_add_explicit(&g->frames, frames, memory_order_relaxed);
  dispatch_semaphore_signal(g->sem);
}

// Capture time (host ns) of ring frame index f.
static int64_t frameNs(Ring *g, uint64_t f, double rate) {
  uint64_t host, af;
  uint32_t s1, s2;
  int spins = 0;
  do {
    s1 = atomic_load_explicit(&g->anchorSeq, memory_order_acquire);
    host = atomic_load_explicit(&g->anchorHost, memory_order_relaxed);
    af = atomic_load_explicit(&g->anchorFrame, memory_order_relaxed);
    s2 = atomic_load_explicit(&g->anchorSeq, memory_order_acquire);
  } while ((s1 != s2 || (s1 & 1)) && ++spins < 1000);
  if (host == 0) return im_now_ns();
  return hostToNs(host) + (int64_t)(((double)(int64_t)(f - af)) * 1e9 / rate);
}

#pragma mark - IO object (one per tap generation)

@interface IMIO : NSObject {
 @public
  Ring ring;
  double rate;
  AudioConverterRef conv; // only when rate != 48 kHz
  float *scratch;
  uint32_t scratchFrames;
  uint64_t convR; // reader-side read index while converting
}
- (instancetype)initWithRate:(double)rate;
@end

static AudioStreamBasicDescription stereoFloat(double rate) {
  AudioStreamBasicDescription f = {0};
  f.mSampleRate = rate;
  f.mFormatID = kAudioFormatLinearPCM;
  f.mFormatFlags = kAudioFormatFlagIsFloat | kAudioFormatFlagIsPacked;
  f.mBytesPerPacket = 8;
  f.mFramesPerPacket = 1;
  f.mBytesPerFrame = 8;
  f.mChannelsPerFrame = 2;
  f.mBitsPerChannel = 32;
  return f;
}

@implementation IMIO
- (instancetype)initWithRate:(double)r {
  if (!(self = [super init])) return nil;
  rate = r;
  uint64_t cap = 1;
  while (cap < (uint64_t)(r * 4)) cap <<= 1; // ~4 s
  ring.cap = cap;
  ring.mask = cap - 1;
  ring.buf = calloc(cap * 2, sizeof(float));
  ring.sem = dispatch_semaphore_create(0);
  scratchFrames = 4096;
  scratch = calloc(scratchFrames * 2, sizeof(float));
  if (fabs(r - IM_AUDIO_SAMPLE_RATE) > 0.5) {
    AudioStreamBasicDescription in = stereoFloat(r), out = stereoFloat(IM_AUDIO_SAMPLE_RATE);
    if (AudioConverterNew(&in, &out, &conv) == noErr) {
      UInt32 q = kAudioConverterQuality_High;
      AudioConverterSetProperty(conv, kAudioConverterSampleRateConverterQuality, sizeof q, &q);
    } else {
      conv = NULL;
    }
  }
  return self;
}
- (void)dealloc {
  if (conv) AudioConverterDispose(conv);
  free(ring.buf);
  free(scratch);
}
@end

static OSStatus convInput(AudioConverterRef c, UInt32 *ioPackets, AudioBufferList *io, AudioStreamPacketDescription **desc,
                          void *ud) {
  IMIO *x = (__bridge IMIO *)ud;
  Ring *g = &x->ring;
  uint64_t w = atomic_load_explicit(&g->w, memory_order_acquire);
  uint64_t avail = w - x->convR;
  if (avail == 0) {
    *ioPackets = 0;
    return 'nodt'; // not an error for the caller: "wait for more input"
  }
  uint64_t start = x->convR & g->mask;
  uint64_t n = *ioPackets;
  if (n > avail) n = avail;
  if (n > x->scratchFrames) n = x->scratchFrames;
  if (n > g->cap - start) n = g->cap - start; // contiguous
  memcpy(x->scratch, g->buf + start * 2, n * 2 * sizeof(float));
  x->convR += n;
  atomic_store_explicit(&g->r, x->convR, memory_order_release);
  io->mNumberBuffers = 1;
  io->mBuffers[0].mNumberChannels = 2;
  io->mBuffers[0].mData = x->scratch;
  io->mBuffers[0].mDataByteSize = (UInt32)(n * 2 * sizeof(float));
  *ioPackets = (UInt32)n;
  return noErr;
}

// Waits until the ring has at least n frames beyond index from. Returns 0, or an im_status.
static int32_t waitFrames(IMIO *x, uint64_t from, uint64_t n, uint64_t deadlineNs) {
  Ring *g = &x->ring;
  for (;;) {
    if (atomic_load(&g->stopped)) return IM_ERR_NOT_STARTED;
    uint64_t w = atomic_load_explicit(&g->w, memory_order_acquire);
    if (w - from >= n) return 0;
    int64_t left = (int64_t)deadlineNs - im_now_ns();
    if (left <= 0) return IM_ERR_TIMEOUT;
    dispatch_semaphore_wait(g->sem, dispatch_time(DISPATCH_TIME_NOW, left < 20000000 ? left : 20000000));
  }
}

static int32_t ioRead(IMIO *x, float *out, int64_t *ns, int32_t timeoutMs) {
  Ring *g = &x->ring;
  uint64_t deadline = (uint64_t)im_now_ns() + (uint64_t)(timeoutMs < 0 ? 0 : timeoutMs) * 1000000ull;
  const uint64_t N = IM_AUDIO_CHUNK_FRAMES;
  if (!x->conv) {
    uint64_t r = atomic_load_explicit(&g->r, memory_order_relaxed);
    int32_t st = waitFrames(x, r, N, deadline);
    if (st) return st;
    for (uint64_t i = 0; i < N; i++) {
      uint64_t idx = (r + i) & g->mask;
      out[i * 2] = g->buf[idx * 2];
      out[i * 2 + 1] = g->buf[idx * 2 + 1];
    }
    if (ns) *ns = frameNs(g, r, x->rate);
    atomic_store_explicit(&g->r, r + N, memory_order_release);
    return (int32_t)N;
  }
  UInt32 produced = 0;
  if (ns) *ns = frameNs(g, x->convR, x->rate);
  while (produced < N) {
    int32_t st = waitFrames(x, x->convR, 64, deadline);
    if (st) return st;
    UInt32 want = (UInt32)N - produced;
    AudioBufferList abl = {.mNumberBuffers = 1};
    abl.mBuffers[0].mNumberChannels = 2;
    abl.mBuffers[0].mData = out + produced * 2;
    abl.mBuffers[0].mDataByteSize = want * 2 * sizeof(float);
    OSStatus s = AudioConverterFillComplexBuffer(x->conv, convInput, (__bridge void *)x, &want, &abl, NULL);
    if (s != noErr && s != 'nodt') {
      setErr([NSString stringWithFormat:@"AudioConverterFillComplexBuffer: %@", osstr(s)]);
      return IM_ERR_INTERNAL;
    }
    produced += want;
  }
  return (int32_t)N;
}

// Read index of the next frame the reader will consume.
static uint64_t readIndex(IMIO *x) {
  return x->conv ? x->convR : atomic_load_explicit(&x->ring.r, memory_order_relaxed);
}

static void setReadIndex(IMIO *x, uint64_t idx) {
  x->convR = idx;
  atomic_store_explicit(&x->ring.r, idx, memory_order_release);
}

// Ring frame index captured at host time tNs, from the latest IO anchor.
static BOOL frameAt(IMIO *x, int64_t tNs, int64_t *idx) {
  Ring *g = &x->ring;
  uint64_t host, af;
  uint32_t s1, s2;
  int spins = 0;
  do {
    s1 = atomic_load_explicit(&g->anchorSeq, memory_order_acquire);
    host = atomic_load_explicit(&g->anchorHost, memory_order_relaxed);
    af = atomic_load_explicit(&g->anchorFrame, memory_order_relaxed);
    s2 = atomic_load_explicit(&g->anchorSeq, memory_order_acquire);
  } while ((s1 != s2 || (s1 & 1)) && ++spins < 1000);
  if (host == 0) return NO;
  *idx = (int64_t)af + llround((double)(tNs - hostToNs(host)) * x->rate / 1e9);
  return YES;
}

#pragma mark - tap generations

// One tap + private aggregate device + IO ring. Exclusion changes go to the
// live tap. When an app that is audible right now is added to or removed from
// the exclusions, the tap would cut it mid-waveform (a click), so a second
// generation is built with the new list and the reader crossfades to it,
// aligned by capture time; then the old one is torn down.
@interface IMGen : NSObject {
 @public
  AudioObjectID tapID, aggID;
  AudioDeviceIOProcID procID;
  CATapDescription *desc;
  AudioStreamBasicDescription fmt;
  IMIO *io;
  NSString *aggKind;
  AudioObjectPropertyListenerBlock fmtBlock;
  int64_t createdNs;
}
@end
@implementation IMGen
@end

static const uint32_t kXfadeFrames = 960; // 20 ms

#pragma mark - engine

@interface IMEngine : NSObject {
 @public
  dispatch_queue_t q;
  os_unfair_lock ioLock;
  IMIO *io, *nextIO;      // what the reader uses (guarded by ioLock; replaced on q)
  pthread_mutex_t readMu; // the single reader and its crossfade state below
  IMIO *xfTo;
  uint32_t xfPos;
  NSMutableArray<NSString *> *appRules;
  NSMutableArray<NSNumber *> *pidRules;
  NSMutableDictionary<NSNumber *, NSNumber *> *micSeen; // pid -> host ns last seen with input running
  NSMutableDictionary<NSNumber *, NSNumber *> *micRpid; // pid -> responsible pid
  BOOL running;
  int32_t mode;
  uint32_t flags;
  IMGen *gen, *pending;
  NSString *deviceName, *deviceUID;
  NSArray<NSNumber *> *curObjs;
  NSArray<NSString *> *curIDs;
  NSSet<NSNumber *> *curExcludedPids;
  NSArray<IMProc *> *lastProcs;
  NSMutableArray<NSDictionary *> *events;
  NSMutableArray<NSString *> *warnings;
  int updates, updateErrors, rebuilds, crossfades, crossfadeAborts;
  double lastUpdateMs, maxUpdateMs;
  dispatch_source_t timer;
  AudioObjectPropertyListenerBlock procBlock, devBlock;
}
@end

@implementation IMEngine
@end

static IMEngine *engine(void) {
  static IMEngine *e;
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    e = [IMEngine new];
    e->q = dispatch_queue_create("isshoni.audio.engine", DISPATCH_QUEUE_SERIAL);
    e->ioLock = OS_UNFAIR_LOCK_INIT;
    pthread_mutex_init(&e->readMu, NULL);
    e->appRules = [NSMutableArray array];
    e->pidRules = [NSMutableArray array];
    e->micSeen = [NSMutableDictionary dictionary];
    e->micRpid = [NSMutableDictionary dictionary];
    e->events = [NSMutableArray array];
    e->warnings = [NSMutableArray array];
  });
  return e;
}

static void logEvent(IMEngine *e, NSString *msg) {
  [e->events addObject:@{@"t_ns" : @(im_now_ns()), @"msg" : msg}];
  if (e->events.count > 300) [e->events removeObjectsInRange:NSMakeRange(0, e->events.count - 300)];
}

static const int64_t kMicStickyNs = 10ll * 1000000000ll;

static void classify(IMEngine *e, NSArray<IMProc *> *procs, uint32_t flags) {
  pid_t me = getpid();
  int64_t now = im_now_ns();
  for (IMProc *p in procs) {
    if (p.micUse) {
      e->micSeen[@(p.pid)] = @(now);
      e->micRpid[@(p.pid)] = @(p.rpid);
    }
  }
  NSMutableSet<NSNumber *> *micGroups = [NSMutableSet set];
  for (NSNumber *pid in e->micSeen.allKeys) {
    if (now - e->micSeen[pid].longLongValue > kMicStickyNs) {
      [e->micSeen removeObjectForKey:pid];
      [e->micRpid removeObjectForKey:pid];
    } else {
      [micGroups addObject:e->micRpid[pid]];
    }
  }
  for (IMProc *p in procs) {
    NSString *why = nil, *ruleID = nil;
    if (p.pid == me) {
      why = @"isshoni itself";
    } else if (p.rpid == me || hasAncestor(p.pid, me)) {
      why = @"isshoni (its helper or XPC service)";
    }
    for (NSNumber *r in e->pidRules) {
      if (why) break;
      pid_t rp = r.intValue;
      if (p.pid == rp || p.rpid == rp || hasAncestor(p.pid, rp)) why = [NSString stringWithFormat:@"instance %d", rp];
    }
    for (NSString *rule in e->appRules) {
      if (why) break;
      if (idMatches(rule, p.bundleID)) {
        why = [NSString stringWithFormat:@"app rule %@", rule];
        ruleID = p.bundleID;
        break;
      }
      for (NSString *bid in p.appIDs)
        if (!why && idMatches(rule, bid)) why = [NSString stringWithFormat:@"app rule %@ (inside %@)", rule, bid];
      for (NSString *bid in p.respIDs)
        if (!why && idMatches(rule, bid))
          why = [NSString stringWithFormat:@"app rule %@ (responsible app %@)", rule, bid];
    }
    if (!why && (flags & IM_FLAG_EXCLUDE_MIC_USERS)) {
      if (e->micSeen[@(p.pid)])
        why = @"uses the microphone";
      else if ([micGroups containsObject:@(p.rpid)] || [micGroups containsObject:@(p.pid)])
        why = @"same app as a microphone user";
    }
    p.excluded = why != nil;
    p.reason = why ?: @"";
    p.ruleBundleID = ruleID;
  }
}

// Bundle IDs for CATapDescription.bundleIDs (macOS 26+): each app rule plus the
// standard Electron/Chromium helper IDs, plus bundle IDs seen matching a rule.
static NSArray<NSString *> *tapBundleIDs(IMEngine *e, NSArray<IMProc *> *procs) {
  NSMutableOrderedSet *ids = [NSMutableOrderedSet orderedSet];
  for (NSString *r in e->appRules) {
    [ids addObject:r];
    for (NSString *suffix in @[ @".helper", @".helper.Renderer", @".helper.GPU", @".helper.Plugin" ])
      [ids addObject:[r stringByAppendingString:suffix]];
  }
  for (IMProc *p in procs)
    if (p.excluded && p.ruleBundleID.length) [ids addObject:p.ruleBundleID];
  return [ids.array sortedArrayUsingSelector:@selector(compare:)];
}

static BOOL useBundleIDs(IMEngine *e) {
  if (e->mode == IM_AUDIO_MODE_ENDPOINT || (e->flags & IM_FLAG_NO_BUNDLE_IDS)) return NO;
  if (@available(macOS 26.0, *)) return YES;
  return NO;
}

static NSDictionary *procJSON(IMProc *p) {
  return @{
    @"pid" : @(p.pid),
    @"ppid" : @(p.ppid),
    @"rpid" : @(p.rpid),
    @"name" : p.name ?: @"",
    @"bundle_id" : p.bundleID ?: @"",
    @"apps" : p.appIDs ?: @[],
    @"responsible" : p.rpid != p.pid ? [NSString stringWithFormat:@"%@ %@", p.respName, p.respIDs.firstObject ?: @""] : @"",
    @"output" : @(p.output),
    @"input" : @(p.input),
    @"mic" : @(p.micUse),
    @"input_devices" : p.inputDevices ?: @[],
    @"excluded" : @(p.excluded),
    @"reason" : p.reason ?: @"",
  };
}

static NSArray *appsJSON(NSArray<IMProc *> *procs) {
  NSMutableArray *out = [NSMutableArray array];
  for (IMProc *p in procs)
    if (p.output || p.input || p.excluded) [out addObject:procJSON(p)];
  return out;
}

static AudioObjectID defaultOutput(void) {
  AudioObjectID dev = kAudioObjectUnknown;
  AudioObjectPropertyAddress a = addrOf(kAudioHardwarePropertyDefaultOutputDevice);
  UInt32 sz = sizeof dev;
  AudioObjectGetPropertyData(kAudioObjectSystemObject, &a, 0, NULL, &sz, &dev);
  return dev;
}

static double nominalRate(AudioObjectID dev) {
  Float64 r = 0;
  AudioObjectPropertyAddress a = addrOf(kAudioDevicePropertyNominalSampleRate);
  UInt32 sz = sizeof r;
  AudioObjectGetPropertyData(dev, &a, 0, NULL, &sz, &r);
  return r;
}

static void setReaderIO(IMEngine *e, IMIO *cur, IMIO *next) {
  os_unfair_lock_lock(&e->ioLock);
  e->io = cur;
  e->nextIO = next;
  os_unfair_lock_unlock(&e->ioLock);
}

// Stops and destroys one generation (q). Readers still holding its IMIO see it stopped.
static void teardownGen(IMEngine *e, IMGen *g) {
  if (!g) return;
  if (g->aggID && g->procID) {
    AudioDeviceStop(g->aggID, g->procID);
    AudioDeviceDestroyIOProcID(g->aggID, g->procID);
  }
  if (g->aggID) AudioHardwareDestroyAggregateDevice(g->aggID);
  if (g->tapID) {
    if (g->fmtBlock) {
      AudioObjectPropertyAddress a = addrOf(kAudioTapPropertyFormat);
      AudioObjectRemovePropertyListenerBlock(g->tapID, &a, e->q, g->fmtBlock);
    }
    AudioHardwareDestroyProcessTap(g->tapID);
  }
  g->aggID = g->tapID = kAudioObjectUnknown;
  g->procID = NULL;
  g->fmtBlock = nil;
  if (g->io) {
    atomic_store(&g->io->ring.stopped, 1);
    dispatch_semaphore_signal(g->io->ring.sem);
  }
}

static void stopAll(IMEngine *e) {
  setReaderIO(e, nil, nil);
  teardownGen(e, e->pending);
  teardownGen(e, e->gen);
  e->pending = e->gen = nil;
}

static void rebuild(IMEngine *e, NSString *why);

// Builds a generation for the current exclusion state (q). Returns nil + *err on failure.
static IMGen *buildGen(IMEngine *e, NSString **err) {
  IMGen *g = [IMGen new];
  g->createdNs = im_now_ns();
  NSString *fail = nil;
  if (@available(macOS 14.2, *)) {
    CATapDescription *d = [[CATapDescription alloc] initStereoGlobalTapButExcludeProcesses:e->curObjs ?: @[]];
    d.name = @"isshoni";
    d.privateTap = YES;
    d.muteBehavior = CATapUnmuted;
    if (@available(macOS 26.0, *)) {
      if (useBundleIDs(e)) {
        d.bundleIDs = e->curIDs ?: @[];
        d.processRestoreEnabled = YES;
      }
    }
    AudioObjectID tap = kAudioObjectUnknown;
    OSStatus st = AudioHardwareCreateProcessTap(d, &tap);
    if (st != noErr) {
      *err = [NSString stringWithFormat:@"AudioHardwareCreateProcessTap: %@", osstr(st)];
      return nil;
    }
    g->tapID = tap;
    g->desc = d;
  } else {
    *err = @"process taps need macOS 14.2+";
    return nil;
  }
  NSString *tapUID = getStr(g->tapID, kAudioTapPropertyUID);
  AudioStreamBasicDescription fmt = {0};
  AudioObjectPropertyAddress fa = addrOf(kAudioTapPropertyFormat);
  UInt32 sz = sizeof fmt;
  OSStatus st = AudioObjectGetPropertyData(g->tapID, &fa, 0, NULL, &sz, &fmt);
  if (st != noErr || !tapUID) {
    fail = [NSString stringWithFormat:@"tap format/uid: %@", osstr(st)];
  } else if (fmt.mFormatID != kAudioFormatLinearPCM || !(fmt.mFormatFlags & kAudioFormatFlagIsFloat) ||
             fmt.mBitsPerChannel != 32) {
    fail = [NSString stringWithFormat:@"unexpected tap format (id %@, flags 0x%x, %u bits)", osstr((OSStatus)fmt.mFormatID),
                                      (unsigned)fmt.mFormatFlags, (unsigned)fmt.mBitsPerChannel];
  }
  g->fmt = fmt;
  AudioObjectID out = defaultOutput();
  if (!fail) {
    e->deviceName = getStr(out, kAudioObjectPropertyName) ?: @"";
    e->deviceUID = getStr(out, kAudioDevicePropertyDeviceUID);
    NSMutableDictionary *agg = [@{
      @kAudioAggregateDeviceNameKey : @"isshoni capture",
      @kAudioAggregateDeviceUIDKey : [NSUUID UUID].UUIDString,
      @kAudioAggregateDeviceIsPrivateKey : @YES,
      @kAudioAggregateDeviceIsStackedKey : @NO,
      @kAudioAggregateDeviceTapAutoStartKey : @YES,
      @kAudioAggregateDeviceTapListKey : @[ @{@kAudioSubTapUIDKey : tapUID, @kAudioSubTapDriftCompensationKey : @YES} ],
    } mutableCopy];
    g->aggKind = @"tap-only";
    if ((e->flags & IM_FLAG_AGG_WITH_OUTPUT) && e->deviceUID) {
      agg[@kAudioAggregateDeviceMainSubDeviceKey] = e->deviceUID;
      agg[@kAudioAggregateDeviceSubDeviceListKey] = @[ @{@kAudioSubDeviceUIDKey : e->deviceUID} ];
      g->aggKind = @"tap+output-device";
    }
    AudioObjectID aggID = kAudioObjectUnknown;
    st = AudioHardwareCreateAggregateDevice((__bridge CFDictionaryRef)agg, &aggID);
    if (st != noErr)
      fail = [NSString stringWithFormat:@"AudioHardwareCreateAggregateDevice: %@", osstr(st)];
    else
      g->aggID = aggID;
  }
  if (!fail) {
    g->io = [[IMIO alloc] initWithRate:fmt.mSampleRate];
    if (fabs(fmt.mSampleRate - IM_AUDIO_SAMPLE_RATE) > 0.5 && !g->io->conv)
      fail = [NSString stringWithFormat:@"no converter for %.0f Hz", fmt.mSampleRate];
  }
  if (!fail) {
    Ring *ring = &g->io->ring;
    UInt32 ch = fmt.mChannelsPerFrame;
    bool nonInter = (fmt.mFormatFlags & kAudioFormatFlagIsNonInterleaved) != 0;
    AudioDeviceIOProcID pid = NULL;
    st = AudioDeviceCreateIOProcIDWithBlock(&pid, g->aggID, NULL,
                                            ^(const AudioTimeStamp *now, const AudioBufferList *inData,
                                              const AudioTimeStamp *inTime, AudioBufferList *outData,
                                              const AudioTimeStamp *outTime) {
                                              ringWrite(ring, inData, inTime, ch, nonInter);
                                            });
    if (st != noErr) {
      fail = [NSString stringWithFormat:@"AudioDeviceCreateIOProcIDWithBlock: %@", osstr(st)];
    } else {
      g->procID = pid;
      st = AudioDeviceStart(g->aggID, pid);
      if (st != noErr) fail = [NSString stringWithFormat:@"AudioDeviceStart: %@", osstr(st)];
    }
  }
  if (fail) {
    teardownGen(e, g);
    *err = fail;
    return nil;
  }
  __weak IMEngine *weak = e;
  __weak IMGen *weakGen = g;
  g->fmtBlock = ^(UInt32 n, const AudioObjectPropertyAddress *a) {
    IMEngine *s = weak;
    IMGen *gg = weakGen;
    if (!s || !gg || !s->running || !gg->tapID) return;
    AudioStreamBasicDescription f = {0};
    UInt32 fsz = sizeof f;
    AudioObjectPropertyAddress fa2 = addrOf(kAudioTapPropertyFormat);
    if (AudioObjectGetPropertyData(gg->tapID, &fa2, 0, NULL, &fsz, &f) == noErr &&
        (f.mSampleRate != gg->fmt.mSampleRate || f.mChannelsPerFrame != gg->fmt.mChannelsPerFrame))
      rebuild(s, [NSString stringWithFormat:@"tap format changed to %.0f Hz / %u ch", f.mSampleRate, (unsigned)f.mChannelsPerFrame]);
  };
  AudioObjectAddPropertyListenerBlock(g->tapID, &fa, e->q, g->fmtBlock);
  logEvent(e, [NSString stringWithFormat:@"tap running: %.0f Hz %u ch%s, %@, output device \"%@\" (%.0f Hz), built in %.1f ms",
                                         fmt.mSampleRate, (unsigned)fmt.mChannelsPerFrame, (fmt.mFormatFlags & kAudioFormatFlagIsNonInterleaved) ? " non-interleaved" : "",
                                         g->aggKind, e->deviceName, nominalRate(out), (double)(im_now_ns() - g->createdNs) / 1e6]);
  return g;
}

static void rebuild(IMEngine *e, NSString *why) {
  if (!e->running) return;
  logEvent(e, [NSString stringWithFormat:@"rebuild: %@", why]);
  stopAll(e);
  e->rebuilds++;
  NSString *err = nil;
  e->gen = buildGen(e, &err);
  if (!e->gen) {
    logEvent(e, [NSString stringWithFormat:@"rebuild failed: %@", err]);
    [e->warnings addObject:[NSString stringWithFormat:@"capture stopped: %@", err]];
    return;
  }
  setReaderIO(e, e->gen->io, nil);
}

// Pushes the current exclusion state to a running tap (q).
static void applyLive(IMEngine *e, IMGen *g, int64_t t0) {
  if (!g || !g->tapID || !g->desc) return;
  g->desc.processes = e->curObjs;
  if (@available(macOS 26.0, *)) {
    if (useBundleIDs(e)) g->desc.bundleIDs = e->curIDs;
  }
  AudioObjectPropertyAddress a = addrOf(kAudioTapPropertyDescription);
  CFTypeRef ref = (__bridge CFTypeRef)g->desc;
  OSStatus st = AudioObjectSetPropertyData(g->tapID, &a, 0, NULL, sizeof ref, &ref);
  double ms = (double)(im_now_ns() - t0) / 1e6;
  e->lastUpdateMs = ms;
  if (ms > e->maxUpdateMs) e->maxUpdateMs = ms;
  if (st == noErr) {
    e->updates++;
    logEvent(e, [NSString stringWithFormat:@"tap updated: %lu processes, %lu bundle IDs kept out (%.1f ms)",
                                           (unsigned long)e->curObjs.count, (unsigned long)e->curIDs.count, ms]);
  } else {
    e->updateErrors++;
    logEvent(e, [NSString stringWithFormat:@"tap update failed: %@; rebuilding", osstr(st)]);
    rebuild(e, @"tap update failed");
  }
}

// The reader finished crossfading to `to`: make it current, drop the old tap (q).
static void promote(IMEngine *e, IMIO *to) {
  if (!e->pending || e->pending->io != to) return;
  IMGen *old = e->gen;
  e->gen = e->pending;
  e->pending = nil;
  setReaderIO(e, e->gen->io, nil);
  teardownGen(e, old);
  e->crossfades++;
  logEvent(e, [NSString stringWithFormat:@"crossfade done: now on the new tap (%.0f ms after the change)",
                                         (double)(im_now_ns() - e->gen->createdNs) / 1e6]);
}

// Recomputes classification and applies exclusion changes (q).
static void reconcile(IMEngine *e, NSString *trigger) {
  if (!e->running) return;
  int64_t t0 = im_now_ns();
  NSArray<IMProc *> *procs = snapshot();
  classify(e, procs, e->flags);
  NSMutableSet<NSNumber *> *wasPlaying = [NSMutableSet set];
  for (IMProc *p in e->lastProcs)
    if (p.output) [wasPlaying addObject:@(p.pid)];
  e->lastProcs = procs;
  if (e->mode == IM_AUDIO_MODE_ENDPOINT) return;
  NSMutableArray<NSNumber *> *objs = [NSMutableArray array];
  NSMutableSet<NSNumber *> *pids = [NSMutableSet set];
  NSString *audible = nil; // an app that changes sides while it is playing
  for (IMProc *p in procs) {
    BOOL was = [e->curExcludedPids containsObject:@(p.pid)];
    if (p.excluded != was && p.output && [wasPlaying containsObject:@(p.pid)])
      audible = p.bundleID.length ? p.bundleID : p.name;
    if (!p.excluded) continue;
    [objs addObject:@(p.obj)];
    [pids addObject:@(p.pid)];
    if (!was)
      logEvent(e, [NSString stringWithFormat:@"keep out pid %d %@ (%@)%@", p.pid, p.bundleID.length ? p.bundleID : p.name,
                                             p.reason, trigger ? [@", on " stringByAppendingString:trigger] : @""]);
  }
  for (NSNumber *pid in e->curExcludedPids)
    if (![pids containsObject:pid]) logEvent(e, [NSString stringWithFormat:@"no longer kept out: pid %@", pid]);
  e->curExcludedPids = pids;
  [objs sortUsingSelector:@selector(compare:)];
  NSArray *ids = useBundleIDs(e) ? tapBundleIDs(e, procs) : @[];
  if ([objs isEqualToArray:e->curObjs ?: @[]] && [ids isEqualToArray:e->curIDs ?: @[]]) return;
  e->curObjs = objs;
  e->curIDs = ids;
  if (!e->gen) return; // starting: the first tap is built from curObjs/curIDs
  if (audible && !e->pending && !(e->flags & IM_FLAG_NO_CROSSFADE)) {
    NSString *err = nil;
    IMGen *g = buildGen(e, &err);
    if (g) {
      e->pending = g;
      setReaderIO(e, e->gen->io, g->io);
      logEvent(e, [NSString stringWithFormat:@"crossfading to a new tap: %@ changed sides while playing", audible]);
      dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 2 * NSEC_PER_SEC), e->q, ^{
        if (e->pending != g) return;
        // No reader picked it up (or the new tap stayed silent): apply the change directly.
        e->pending = nil;
        setReaderIO(e, e->gen->io, nil);
        teardownGen(e, g);
        e->crossfadeAborts++;
        logEvent(e, @"crossfade not picked up within 2 s; applying the change to the live tap");
        applyLive(e, e->gen, im_now_ns());
      });
      return;
    }
    logEvent(e, [NSString stringWithFormat:@"crossfade unavailable (%@); switching directly", err]);
  }
  // During a crossfade the old tap keeps its list (changing it would cut an app
  // in or out mid-waveform, audible before the fade completes); it is gone in ~0.1 s.
  applyLive(e, e->pending ?: e->gen, t0);
}

#pragma mark - reader

// Best alignment of b against a (left channel) within ±20 samples, for diagnostics.
static void bestLag(const float *a, const float *b, int *lag, float *corr) {
  const int N = IM_AUDIO_CHUNK_FRAMES, M = 20;
  double best = -2;
  int bl = 0;
  for (int L = -M; L <= M; L++) {
    double ab = 0, aa = 0, bb = 0;
    for (int i = M; i < N - M; i++) {
      double x = a[i * 2], y = b[(i + L) * 2];
      ab += x * y;
      aa += x * x;
      bb += y * y;
    }
    double c = (aa > 0 && bb > 0) ? ab / sqrt(aa * bb) : 0;
    if (c > best) {
      best = c;
      bl = L;
    }
  }
  *lag = bl;
  *corr = (float)best;
}

// Starts a crossfade to nxt at the capture time the reader has reached in cur.
static void tryStartCrossfade(IMEngine *e, IMIO *cur, IMIO *nxt) {
  int64_t t = frameNs(&cur->ring, readIndex(cur), cur->rate);
  int64_t idx;
  if (!frameAt(nxt, t, &idx) || idx < 0) return; // the new tap started after that moment: keep going on cur
  // Each tap has its own IO thread; the new one's callback may land a little
  // after the old one's, so it can be a buffer behind the reader: wait for it.
  int64_t until = im_now_ns() + 50000000;
  while ((int64_t)atomic_load_explicit(&nxt->ring.w, memory_order_acquire) < idx + IM_AUDIO_CHUNK_FRAMES) {
    if (im_now_ns() > until || atomic_load(&nxt->ring.stopped)) return;
    dispatch_semaphore_wait(nxt->ring.sem, dispatch_time(DISPATCH_TIME_NOW, 5 * NSEC_PER_MSEC));
  }
  setReadIndex(nxt, (uint64_t)idx);
  e->xfTo = nxt;
  e->xfPos = 0;
}

static int32_t readLocked(IMEngine *e, float *out, int64_t *ns, int64_t deadline) {
  for (;;) {
    os_unfair_lock_lock(&e->ioLock);
    IMIO *cur = e->io, *nxt = e->nextIO;
    os_unfair_lock_unlock(&e->ioLock);
    if (!cur) { // between generations during a rebuild, or not started
      if (im_now_ns() >= deadline) return IM_ERR_NOT_STARTED;
      usleep(5000);
      continue;
    }
    if (e->xfTo && (e->xfTo == cur || e->xfTo != nxt)) e->xfTo = nil; // promoted, or the switch was abandoned
    if (!e->xfTo && nxt) tryStartCrossfade(e, cur, nxt);
    int32_t left = (int32_t)((deadline - im_now_ns()) / 1000000);
    if (left < 0) left = 0;
    IMIO *src = (e->xfTo && e->xfPos >= kXfadeFrames) ? e->xfTo : cur; // faded over, awaiting promotion
    int32_t n = ioRead(src, out, ns, left);
    if (n == IM_ERR_NOT_STARTED && im_now_ns() < deadline) continue; // generation replaced: retry
    if (n < 0 || !e->xfTo || src == e->xfTo) return n;
    float b[IM_AUDIO_CHUNK_FRAMES * 2];
    int64_t nsb;
    if (ioRead(e->xfTo, b, &nsb, left) < 0) {
      e->xfTo = nil; // the new tap failed: stay on the old one
      return n;
    }
    if (e->xfPos == 0) {
      int lag;
      float corr;
      bestLag(out, b, &lag, &corr);
      dispatch_async(e->q, ^{
        logEvent(e, [NSString stringWithFormat:@"crossfade: taps aligned by capture time, measured offset %d samples (correlation %.2f)", lag, corr]);
      });
    }
    for (uint32_t i = 0; i < IM_AUDIO_CHUNK_FRAMES; i++) {
      float g = 0.5f - 0.5f * cosf((float)M_PI * (float)(e->xfPos + i) / (float)kXfadeFrames);
      out[i * 2] = out[i * 2] * (1 - g) + b[i * 2] * g;
      out[i * 2 + 1] = out[i * 2 + 1] * (1 - g) + b[i * 2 + 1] * g;
    }
    e->xfPos += IM_AUDIO_CHUNK_FRAMES;
    if (e->xfPos >= kXfadeFrames) {
      IMIO *to = e->xfTo;
      dispatch_async(e->q, ^{ promote(e, to); });
    }
    return n;
  }
}

#pragma mark - C ABI

int32_t im_version(void) { return IM_ABI_VERSION; }

int32_t im_last_error(char *buf, int32_t cap) {
  if (!buf || cap <= 0) return IM_ERR_INVALID_ARG;
  strlcpy(buf, tlErr, (size_t)cap);
  return (int32_t)strlen(buf);
}

// Private TCC preflight, diagnostics only: 0 granted, 1 denied, 2 not asked yet, -1 unknown.
static int tccPreflight(void) {
  static int (*fn)(CFStringRef, CFDictionaryRef);
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    void *h = dlopen("/System/Library/PrivateFrameworks/TCC.framework/Versions/A/TCC", RTLD_LAZY);
    if (h) fn = (int (*)(CFStringRef, CFDictionaryRef))dlsym(h, "TCCAccessPreflight");
  });
  return fn ? fn(CFSTR("kTCCServiceAudioCapture"), NULL) : -1;
}

int32_t im_audio_probe(char *json, int32_t cap) {
  @autoreleasepool {
    NSOperatingSystemVersion v = NSProcessInfo.processInfo.operatingSystemVersion;
    BOOL tap = NO, ids = NO;
    if (@available(macOS 14.2, *)) tap = YES;
    if (@available(macOS 26.0, *)) ids = YES;
    AudioObjectID out = defaultOutput();
    initSPI();
    return writeJSON(@{
      @"os" : [NSString stringWithFormat:@"%ld.%ld.%ld", (long)v.majorVersion, (long)v.minorVersion, (long)v.patchVersion],
      @"process_tap" : @(tap),
      @"bundle_ids" : @(ids),
      @"responsible_pid_spi" : [NSNumber numberWithBool:respFn != NULL],
      @"tcc_audio_capture" : @(tccPreflight()),
      @"device" : getStr(out, kAudioObjectPropertyName) ?: @"",
      @"device_rate" : @(nominalRate(out)),
      @"error" : tap ? @"" : @"process taps need macOS 14.2+",
    },
                     json, cap);
  }
}

int32_t im_audio_clear_rules(void) {
  IMEngine *e = engine();
  dispatch_sync(e->q, ^{
    [e->appRules removeAllObjects];
    [e->pidRules removeAllObjects];
    reconcile(e, @"rules changed");
  });
  return IM_OK;
}

int32_t im_audio_add_rule(int32_t kind, const char *value) {
  if (!value || !*value) {
    setErr(@"empty rule");
    return IM_ERR_INVALID_ARG;
  }
  NSString *v = [NSString stringWithUTF8String:value];
  if (!v) {
    setErr(@"rule is not UTF-8");
    return IM_ERR_INVALID_ARG;
  }
  IMEngine *e = engine();
  if (kind == IM_RULE_APP) {
    dispatch_sync(e->q, ^{
      if (![e->appRules containsObject:v]) [e->appRules addObject:v];
      reconcile(e, @"rules changed");
    });
    return IM_OK;
  }
  if (kind == IM_RULE_INSTANCE) {
    int pid = v.intValue;
    if (pid <= 1) {
      setErr(@"instance rule needs a pid > 1");
      return IM_ERR_INVALID_ARG;
    }
    dispatch_sync(e->q, ^{
      if (![e->pidRules containsObject:@(pid)]) [e->pidRules addObject:@(pid)];
      reconcile(e, @"rules changed");
    });
    return IM_OK;
  }
  setErr(@"unknown rule kind");
  return IM_ERR_INVALID_ARG;
}

int32_t im_audio_list_apps(uint32_t flags, char *json, int32_t cap) {
  IMEngine *e = engine();
  __block int32_t n = 0;
  __block NSString *err = nil;
  dispatch_sync(e->q, ^{
    @autoreleasepool {
      NSArray *procs = snapshot();
      classify(e, procs, flags);
      NSMutableArray *all = [NSMutableArray array];
      for (IMProc *p in procs) [all addObject:procJSON(p)];
      n = writeJSON(@{@"apps" : all}, json, cap);
      if (n == IM_ERR_INTERNAL) err = [NSString stringWithUTF8String:tlErr];
    }
  });
  if (err) setErr(err);
  return n;
}

int32_t im_audio_start(int32_t mode, uint32_t flags) {
  if (mode != IM_AUDIO_MODE_INCLUDE_SET && mode != IM_AUDIO_MODE_ENDPOINT) {
    setErr(@"mode not supported on macOS");
    return IM_ERR_INVALID_ARG;
  }
  if (@available(macOS 14.2, *)) {
  } else {
    setErr(@"process taps need macOS 14.2+");
    return IM_ERR_UNSUPPORTED_OS;
  }
  IMEngine *e = engine();
  __block int32_t rc = IM_OK;
  __block NSString *err = nil;
  dispatch_sync(e->q, ^{
    @autoreleasepool {
      if (e->running) {
        rc = IM_ERR_ALREADY_STARTED;
        err = @"already started";
        return;
      }
      e->mode = mode;
      e->flags = flags;
      e->running = YES;
      e->updates = e->updateErrors = e->rebuilds = e->crossfades = e->crossfadeAborts = 0;
      e->lastUpdateMs = e->maxUpdateMs = 0;
      e->curObjs = @[];
      e->curIDs = @[];
      e->curExcludedPids = [NSSet set];
      e->lastProcs = nil;
      [e->events removeAllObjects];
      [e->warnings removeAllObjects];
      reconcile(e, @"start"); // classify first so the tap starts with the right exclusions
      e->gen = buildGen(e, &err);
      if (!e->gen) {
        e->running = NO;
        rc = IM_ERR_INTERNAL;
        return;
      }
      setReaderIO(e, e->gen->io, nil);
      __weak IMEngine *weak = e;
      e->procBlock = ^(UInt32 n, const AudioObjectPropertyAddress *a) {
        IMEngine *s = weak;
        if (s) reconcile(s, @"process list change");
      };
      AudioObjectPropertyAddress pa = addrOf(kAudioHardwarePropertyProcessObjectList);
      AudioObjectAddPropertyListenerBlock(kAudioObjectSystemObject, &pa, e->q, e->procBlock);
      e->devBlock = ^(UInt32 n, const AudioObjectPropertyAddress *a) {
        IMEngine *s = weak;
        if (s) rebuild(s, @"default output device changed");
      };
      AudioObjectPropertyAddress da = addrOf(kAudioHardwarePropertyDefaultOutputDevice);
      AudioObjectAddPropertyListenerBlock(kAudioObjectSystemObject, &da, e->q, e->devBlock);
      e->timer = dispatch_source_create(DISPATCH_SOURCE_TYPE_TIMER, 0, 0, e->q);
      dispatch_source_set_timer(e->timer, dispatch_time(DISPATCH_TIME_NOW, 250 * NSEC_PER_MSEC), 250 * NSEC_PER_MSEC,
                                20 * NSEC_PER_MSEC);
      dispatch_source_set_event_handler(e->timer, ^{
        IMEngine *s = weak;
        if (s) reconcile(s, nil);
      });
      dispatch_resume(e->timer);
    }
  });
  if (err) setErr(err);
  return rc;
}

void im_audio_stop(void) {
  IMEngine *e = engine();
  dispatch_sync(e->q, ^{
    if (!e->running) return;
    e->running = NO;
    if (e->timer) dispatch_source_cancel(e->timer);
    e->timer = nil;
    AudioObjectPropertyAddress pa = addrOf(kAudioHardwarePropertyProcessObjectList);
    if (e->procBlock) AudioObjectRemovePropertyListenerBlock(kAudioObjectSystemObject, &pa, e->q, e->procBlock);
    AudioObjectPropertyAddress da = addrOf(kAudioHardwarePropertyDefaultOutputDevice);
    if (e->devBlock) AudioObjectRemovePropertyListenerBlock(kAudioObjectSystemObject, &da, e->q, e->devBlock);
    e->procBlock = e->devBlock = nil;
    stopAll(e);
  });
}

int32_t im_audio_read(float *out, int32_t max_frames, int64_t *capture_ns, int32_t timeout_ms) {
  if (!out || max_frames < IM_AUDIO_CHUNK_FRAMES) {
    setErr(@"buffer too small (need 480 frames)");
    return IM_ERR_INVALID_ARG;
  }
  IMEngine *e = engine();
  int64_t deadline = im_now_ns() + (int64_t)timeout_ms * 1000000;
  pthread_mutex_lock(&e->readMu);
  int32_t n = readLocked(e, out, capture_ns, deadline);
  pthread_mutex_unlock(&e->readMu);
  if (n == IM_ERR_TIMEOUT) setErr(@"timeout");
  if (n == IM_ERR_NOT_STARTED) setErr(@"not capturing");
  return n;
}

int32_t im_audio_status(char *json, int32_t cap) {
  IMEngine *e = engine();
  __block int32_t n = 0;
  __block NSString *err = nil;
  dispatch_sync(e->q, ^{
    @autoreleasepool {
      IMGen *g = e->gen;
      IMIO *io = g ? g->io : nil;
      NSMutableArray *warn = [e->warnings mutableCopy];
      NSMutableDictionary *d = [@{
        @"running" : @(e->running),
        @"mode" : @(e->mode),
        @"flags" : @(e->flags),
        @"device" : e->deviceName ?: @"",
        @"aggregate" : (g ? g->aggKind : nil) ?: @"",
        @"bundle_ids" : e->curIDs ?: @[],
        @"excluded_objects" : @(e->curObjs.count),
        @"apps" : appsJSON(e->lastProcs ?: @[]),
        @"events" : [e->events copy],
        @"updates" : @(e->updates),
        @"update_errors" : @(e->updateErrors),
        @"rebuilds" : @(e->rebuilds),
        @"crossfades" : @(e->crossfades),
        @"crossfade_aborts" : @(e->crossfadeAborts),
        @"last_update_ms" : @(e->lastUpdateMs),
        @"max_update_ms" : @(e->maxUpdateMs),
      } mutableCopy];
      if (io) {
        Ring *r = &io->ring;
        double silentMs = (double)atomic_load(&r->zeroRun) * 1000.0 / io->rate;
        d[@"tap_rate"] = @(io->rate);
        d[@"tap_channels"] = @(g->fmt.mChannelsPerFrame);
        d[@"callbacks"] = @(atomic_load(&r->callbacks));
        d[@"frames"] = @(atomic_load(&r->frames));
        d[@"overflow_frames"] = @(atomic_load(&r->overflow));
        d[@"max_peak"] = @(peakGet(&r->maxPeak, NO));
        d[@"peak"] = @(peakGet(&r->recentPeak, YES));
        d[@"silent_ms"] = @(silentMs);
        if (atomic_load(&r->callbacks) == 0 && e->running)
          [warn addObject:@"the capture device has not delivered any audio yet"];
        else if (silentMs > 3000)
          [warn addObject:@"only digital silence for over 3 s: nothing is playing, or System Audio Recording permission is "
                          @"missing"];
      }
      d[@"warnings"] = warn;
      n = writeJSON(d, json, cap);
      if (n == IM_ERR_INTERNAL) err = [NSString stringWithUTF8String:tlErr];
    }
  });
  if (err) setErr(err);
  return n;
}

#pragma mark - test hook

// Pushes in (interleaved stereo at in_rate) through the same ring + converter
// path as live capture and returns 48 kHz frames written to out (for go test).
int32_t im_test_resample(const float *in, int32_t in_frames, double in_rate, float *out, int32_t out_cap_frames) {
  IMIO *x = [[IMIO alloc] initWithRate:in_rate];
  if (in_frames > (int32_t)x->ring.cap) return IM_ERR_INVALID_ARG;
  memcpy(x->ring.buf, in, (size_t)in_frames * 2 * sizeof(float));
  atomic_store(&x->ring.w, (uint64_t)in_frames);
  int32_t total = 0;
  while (total + IM_AUDIO_CHUNK_FRAMES <= out_cap_frames) {
    int64_t ns = 0;
    int32_t n = ioRead(x, out + (size_t)total * 2, &ns, 0);
    if (n <= 0) break;
    total += n;
  }
  return total;
}
