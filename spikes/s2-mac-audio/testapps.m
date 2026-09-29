// testapps.m: sound sources for the S2 self-test (fake apps and isshoni's own
// playback): a sine tone via AudioQueue, an open microphone, and a tone played
// by a WKWebView (its audio comes from WebKit's GPU process, not from us).

#import <AppKit/AppKit.h>
#import <AudioToolbox/AudioToolbox.h>
#import <WebKit/WebKit.h>
#include <stdatomic.h>

#include "testapps.h"

typedef struct {
  double phase, inc, amp;
  uint32_t fadeIn, fadeInLen;
  _Atomic int stopping; // 1: fade out, then silence
  uint32_t fadeOut, fadeOutLen;
} Tone;

static _Atomic int gStop;

void im_test_stop(void) { atomic_store(&gStop, 1); }

static void toneFill(void *ud, AudioQueueRef q, AudioQueueBufferRef b) {
  Tone *t = ud;
  float *p = b->mAudioData;
  UInt32 n = b->mAudioDataBytesCapacity / (2 * sizeof(float));
  int stopping = atomic_load(&t->stopping);
  for (UInt32 i = 0; i < n; i++) {
    double env = 1;
    if (t->fadeIn < t->fadeInLen) env = (double)(++t->fadeIn) / t->fadeInLen;
    if (stopping) env *= t->fadeOut < t->fadeOutLen ? 1.0 - (double)(++t->fadeOut) / t->fadeOutLen : 0;
    float s = (float)(t->amp * env * sin(t->phase));
    t->phase += t->inc;
    if (t->phase > 2 * M_PI) t->phase -= 2 * M_PI;
    p[i * 2] = p[i * 2 + 1] = s;
  }
  b->mAudioDataByteSize = n * 2 * sizeof(float);
  AudioQueueEnqueueBuffer(q, b, 0, NULL);
}

static void micDiscard(void *ud, AudioQueueRef q, AudioQueueBufferRef b, const AudioTimeStamp *t, UInt32 n,
                       const AudioStreamPacketDescription *d) {
  AudioQueueEnqueueBuffer(q, b, 0, NULL);
}

static AudioStreamBasicDescription fmt48(void) {
  AudioStreamBasicDescription f = {0};
  f.mSampleRate = 48000;
  f.mFormatID = kAudioFormatLinearPCM;
  f.mFormatFlags = kAudioFormatFlagIsFloat | kAudioFormatFlagIsPacked;
  f.mBytesPerPacket = f.mBytesPerFrame = 8;
  f.mFramesPerPacket = 1;
  f.mChannelsPerFrame = 2;
  f.mBitsPerChannel = 32;
  return f;
}

static int fail(char *err, int cap, const char *what, OSStatus st) {
  if (err && cap > 0) snprintf(err, (size_t)cap, "%s: %d", what, (int)st);
  return -1;
}

int im_test_tone(double freq, double amp, double seconds, int hold_mic, char *err, int cap) {
  AudioStreamBasicDescription f = fmt48();
  AudioQueueRef mic = NULL;
  if (hold_mic) {
    OSStatus st = AudioQueueNewInput(&f, micDiscard, NULL, NULL, NULL, 0, &mic);
    if (st) return fail(err, cap, "AudioQueueNewInput", st);
    for (int i = 0; i < 3; i++) {
      AudioQueueBufferRef b;
      if (AudioQueueAllocateBuffer(mic, 4800 * 8, &b) == noErr) AudioQueueEnqueueBuffer(mic, b, 0, NULL);
    }
    st = AudioQueueStart(mic, NULL);
    if (st) return fail(err, cap, "AudioQueueStart(mic)", st);
  }
  Tone t = {.inc = 2 * M_PI * freq / 48000, .amp = amp, .fadeInLen = 240, .fadeOutLen = 240};
  AudioQueueRef q = NULL;
  OSStatus st = freq > 0 ? AudioQueueNewOutput(&f, toneFill, &t, NULL, NULL, 0, &q) : noErr;
  if (st) return fail(err, cap, "AudioQueueNewOutput", st);
  if (q) {
    for (int i = 0; i < 3; i++) {
      AudioQueueBufferRef b;
      if (AudioQueueAllocateBuffer(q, 480 * 8, &b) == noErr) toneFill(&t, q, b);
    }
    st = AudioQueueStart(q, NULL);
    if (st) return fail(err, cap, "AudioQueueStart", st);
  }
  for (double waited = 0; waited < seconds && !atomic_load(&gStop); waited += 0.01) usleep(10000);
  if (q) {
    atomic_store(&t.stopping, 1);
    usleep(60000); // let the fade-out play
    AudioQueueStop(q, true);
    AudioQueueDispose(q, true);
  }
  if (mic) {
    AudioQueueStop(mic, true);
    AudioQueueDispose(mic, true);
  }
  return 0;
}

// Must run on the main thread. Plays freq through a WKWebView (WebAudio) for
// seconds, pumping the run loop, and reports the AudioContext state.
int im_test_webview_tone(double freq, double seconds, char *state, int cap) {
  @autoreleasepool {
    if (!NSThread.isMainThread) {
      snprintf(state, (size_t)cap, "not on the main thread");
      return -1;
    }
    [NSApplication sharedApplication];
    [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
    WKWebViewConfiguration *cfg = [WKWebViewConfiguration new];
    cfg.mediaTypesRequiringUserActionForPlayback = WKAudiovisualMediaTypeNone;
    NSWindow *w = [[NSWindow alloc] initWithContentRect:NSMakeRect(-4000, -4000, 160, 120)
                                              styleMask:NSWindowStyleMaskBorderless
                                                backing:NSBackingStoreBuffered
                                                  defer:NO];
    w.releasedWhenClosed = NO;
    WKWebView *wv = [[WKWebView alloc] initWithFrame:NSMakeRect(0, 0, 160, 120) configuration:cfg];
    w.contentView = wv;
    [w orderFront:nil];
    NSString *html = [NSString
        stringWithFormat:@"<!doctype html><script>const c=new AudioContext();const o=c.createOscillator();const g=c.createGain();"
                         @"o.frequency.value=%f;g.gain.value=0.25;o.connect(g).connect(c.destination);o.start();</script>",
                         freq];
    [wv loadHTMLString:html baseURL:nil];
    __block NSString *last = @"(no reply)";
    NSDate *end = [NSDate dateWithTimeIntervalSinceNow:seconds];
    NSDate *nextPoll = [NSDate date];
    while ([end timeIntervalSinceNow] > 0) {
      [[NSRunLoop mainRunLoop] runMode:NSDefaultRunLoopMode beforeDate:[NSDate dateWithTimeIntervalSinceNow:0.05]];
      if ([nextPoll timeIntervalSinceNow] <= 0) {
        nextPoll = [NSDate dateWithTimeIntervalSinceNow:0.5];
        [wv evaluateJavaScript:@"typeof c === 'undefined' ? 'no context' : c.state"
             completionHandler:^(id r, NSError *e) {
               last = e ? [NSString stringWithFormat:@"error: %@", e.localizedDescription] : [NSString stringWithFormat:@"%@", r];
             }];
      }
    }
    [wv evaluateJavaScript:@"c.close()" completionHandler:nil];
    NSDate *drain = [NSDate dateWithTimeIntervalSinceNow:0.3];
    while ([drain timeIntervalSinceNow] > 0)
      [[NSRunLoop mainRunLoop] runMode:NSDefaultRunLoopMode beforeDate:[NSDate dateWithTimeIntervalSinceNow:0.05]];
    [w close];
    snprintf(state, (size_t)cap, "%s", last.UTF8String);
    return 0;
  }
}
