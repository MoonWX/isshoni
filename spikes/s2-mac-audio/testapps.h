// Sound sources for the S2 self-test (see testapps.m).
#ifndef ISSHONI_S2_TESTAPPS_H
#define ISSHONI_S2_TESTAPPS_H

#include <stdint.h>

/* Plays a sine (48 kHz stereo float) for seconds; freq 0 = no tone (mic only).
 * hold_mic also keeps the default microphone open. Returns 0 or -1 with err. */
int im_test_tone(double freq, double amp, double seconds, int hold_mic, char *err, int cap);
/* Ends a running im_test_tone early (with a fade). */
void im_test_stop(void);
/* Main thread only: plays freq via WKWebView WebAudio for seconds. */
int im_test_webview_tone(double freq, double seconds, char *state, int cap);
/* Resampler check (tap_engine.m): in_rate → 48 kHz through the live read path. */
int32_t im_test_resample(const float *in, int32_t in_frames, double in_rate, float *out, int32_t out_cap_frames);

#endif
