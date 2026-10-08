// The WebAudio side of the sharer's level meter (05 §13.7): one AudioContext per page, made in the Share click, and
// an AnalyserNode per metered track. Nothing is ever connected to the context's destination: the captured sound is
// measured, never played back to the sharer.
//
// Why "in the Share click": a browser lets an AudioContext run only once the page had a user gesture, and Safari
// only when it is created or resumed inside one. The Share click is that gesture (the click that opens the picker),
// so ShareButton calls primeLevelAudio() there. From then on the context may run whenever it is asked to.
//
// It runs only while something is metered. The click's resume() is what unlocks it; as soon as that has happened it
// is suspended again, until a meter opens (the share panel's settings), and again when the last meter closes. A
// sharer who never opens the settings, or shares without sound, has no audio thread and no open output device left
// behind by the click.

type AudioContextCtor = new () => AudioContext;

let context: AudioContext | null = null;
/** The open taps: the context runs while there are some and is suspended while there are none. */
let taps = 0;

/** The page's AudioContext, made on first use; null where WebAudio is missing or refuses. */
function ensureContext(): AudioContext | null {
  if (context !== null) return context;
  const Ctor = (globalThis as { AudioContext?: AudioContextCtor }).AudioContext;
  if (typeof Ctor !== 'function') return null;
  try {
    context = new Ctor();
  } catch {
    context = null;
  }
  return context;
}

/**
 * Asks the context for the state the taps need: running while something is metered, suspended otherwise. It asks
 * every time instead of reading `state` first: the state follows a resume() or suspend() only later, and of the
 * requests a context got, the last one counts.
 */
function settle(ctx: AudioContext): void {
  if (ctx.state === 'closed') return;
  try {
    void (taps > 0 ? ctx.resume() : ctx.suspend()).catch(() => undefined);
  } catch {
    // A context that can't be suspended or resumed stays as it is; the meter then reads what it gets.
  }
}

/**
 * Creates the page's AudioContext and unlocks it: resume() inside a user gesture. Call it from a click handler (the
 * Share click). While nothing is metered the context is suspended again once it has run. A no-op where WebAudio is
 * missing; the level meter then shows nothing.
 */
export function primeLevelAudio(): void {
  const ctx = ensureContext();
  if (ctx === null || ctx.state === 'closed') return;
  try {
    // Also when it runs already: a context made inside a gesture may start by itself, and that one is suspended
    // just the same when nothing is metered.
    void ctx.resume().then(
      () => {
        settle(ctx);
      },
      () => undefined,
    );
  } catch {
    // As in settle().
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
  const ctx = ensureContext();
  if (ctx === null || typeof globalThis.MediaStream !== 'function') return null;
  try {
    const source = ctx.createMediaStreamSource(new MediaStream([track]));
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 1024;
    source.connect(analyser);
    const samples = new Float32Array(analyser.fftSize);
    taps++;
    settle(ctx);
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
        // The last one: nothing to measure, no reason to keep the audio thread running. The next tap resumes it.
        if (taps === 0) settle(ctx);
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
