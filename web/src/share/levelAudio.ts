// The WebAudio side of the sharer's level meter (05 §13.7): one AudioContext per page, made in the Share click, and
// an AnalyserNode per metered track. Nothing is ever connected to the context's destination: the captured sound is
// measured, never played back to the sharer.
//
// Why "in the Share click": a browser lets an AudioContext run only once the page had a user gesture, and Safari
// only when it is created or resumed inside one. The Share click is that gesture (the click that opens the picker),
// so ShareButton calls primeLevelAudio() there; a meter that opens later finds the context running.

type AudioContextCtor = new () => AudioContext;

let context: AudioContext | null = null;
/** The open taps: the context is suspended while there are none. */
let taps = 0;

function contextCtor(): AudioContextCtor | undefined {
  const ctor = (globalThis as { AudioContext?: AudioContextCtor }).AudioContext;
  return typeof ctor === 'function' ? ctor : undefined;
}

function resume(ctx: AudioContext): void {
  if (ctx.state === 'suspended') void ctx.resume().catch(() => undefined);
}

/**
 * Creates the page's AudioContext, or resumes it. Call it from a click handler (the Share click). A no-op where
 * WebAudio is missing; the level meter then shows nothing.
 */
export function primeLevelAudio(): void {
  const Ctor = contextCtor();
  if (Ctor === undefined) return;
  try {
    context ??= new Ctor();
    resume(context);
  } catch {
    context = null;
  }
}

export interface LevelTap {
  /** The peak of the newest samples (about 20 ms), 0 to 1. Digital silence is exactly 0. */
  read(): number;
  /** Disconnects the analyser. Safe to call twice. */
  close(): void;
}

/**
 * Starts measuring an audio track; null where that isn't possible (no WebAudio, or the browser refuses the track).
 * The track is neither cloned nor changed, and what it carries is not played.
 */
export function openLevelTap(track: MediaStreamTrack): LevelTap | null {
  primeLevelAudio();
  const ctx = context;
  if (ctx === null || typeof globalThis.MediaStream !== 'function') return null;
  try {
    const source = ctx.createMediaStreamSource(new MediaStream([track]));
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 1024;
    source.connect(analyser);
    const samples = new Float32Array(analyser.fftSize);
    taps++;
    resume(ctx);
    let open = true;
    return {
      read() {
        if (!open) return 0;
        analyser.getFloatTimeDomainData(samples);
        let peak = 0;
        for (const v of samples) peak = Math.max(peak, Math.abs(v));
        return Math.min(1, peak);
      },
      close() {
        if (!open) return;
        open = false;
        try {
          source.disconnect();
        } catch {
          // Already disconnected.
        }
        taps--;
        // Nothing to measure: no reason to keep the audio thread running. The next tap resumes it.
        if (taps === 0 && ctx.state === 'running') void ctx.suspend().catch(() => undefined);
      },
    };
  } catch {
    return null;
  }
}

/** Forgets the page's AudioContext (tests). */
export function resetLevelAudio(): void {
  const ctx = context;
  context = null;
  taps = 0;
  if (ctx !== null && ctx.state !== 'closed') void ctx.close().catch(() => undefined);
}
