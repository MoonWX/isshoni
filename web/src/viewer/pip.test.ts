// Picture-in-picture (05 §12.5): the stage's video in the browser's floating window, only where the platform says
// it can; the store follows the window, and the window follows the store.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createPip, type PipController } from './pip';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { room, SELF, shareInfo } from './testing';

let viewer: ViewerServices;
let stage: HTMLElement;
let video: HTMLVideoElement;
let detach: (() => void) | undefined;

/** The browser's PiP: the element in the window, the calls, and the events it fires at the video. */
interface FakePip {
  element: Element | null;
  request: ReturnType<typeof vi.fn<() => Promise<unknown>>>;
  exit: ReturnType<typeof vi.fn<() => Promise<void>>>;
  /** The window's own close button. */
  close(): void;
}

function installPip(): FakePip {
  const pip: FakePip = {
    element: null,
    request: vi.fn(),
    exit: vi.fn(),
    close() {
      const el = pip.element;
      pip.element = null;
      el?.dispatchEvent(new Event('leavepictureinpicture', { bubbles: true }));
    },
  };
  pip.request.mockImplementation(function (this: Element) {
    pip.element = this;
    this.dispatchEvent(new Event('enterpictureinpicture', { bubbles: true }));
    return Promise.resolve({});
  });
  pip.exit.mockImplementation(() => {
    pip.close();
    return Promise.resolve();
  });
  Object.defineProperty(document, 'pictureInPictureElement', { configurable: true, get: () => pip.element });
  Object.defineProperty(document, 'exitPictureInPicture', { configurable: true, value: pip.exit });
  Object.defineProperty(HTMLVideoElement.prototype, 'requestPictureInPicture', {
    configurable: true,
    value: pip.request,
  });
  return pip;
}

function controller(supported = true): PipController {
  const pip = createPip({
    store: viewer.store,
    supported,
    target: () => ({
      video: stage.isConnected ? stage.querySelector('video') : null,
      shareId: stage.isConnected ? (stage.getAttribute('data-share-id') ?? null) : null,
    }),
  });
  detach = pip.attach();
  return pip;
}

const inPip = (): string | null => viewer.store.getState().pipShareId;

beforeEach(() => {
  viewer = createViewer();
  syncRoom(viewer, room(shareInfo('s_bea', 'u_bea', 1), shareInfo('s_cy', 'u_cy', 2)), SELF);
  stage = document.createElement('section');
  stage.setAttribute('data-share-id', 's_cy');
  video = document.createElement('video');
  stage.append(video);
  document.body.append(stage);
});

afterEach(() => {
  detach?.();
  detach = undefined;
  viewer.dispose();
  Reflect.deleteProperty(document, 'pictureInPictureElement');
  Reflect.deleteProperty(document, 'exitPictureInPicture');
  Reflect.deleteProperty(HTMLVideoElement.prototype, 'requestPictureInPicture');
  document.body.replaceChildren();
});

describe('createPip', () => {
  it('puts the stage’s video in the window and tells the store which share it is', () => {
    const browser = installPip();
    const pip = controller();
    pip.enter();
    expect(browser.request).toHaveBeenCalledOnce();
    expect(browser.request.mock.contexts[0]).toBe(video);
    expect(inPip()).toBe('s_cy');

    pip.exit();
    expect(browser.exit).toHaveBeenCalledOnce();
    expect(inPip()).toBeNull();
  });

  it('toggles', () => {
    installPip();
    const pip = controller();
    pip.toggle();
    expect(inPip()).toBe('s_cy');
    pip.toggle();
    expect(inPip()).toBeNull();
  });

  it('sees the window’s own close button', () => {
    const browser = installPip();
    controller().enter();
    browser.close();
    expect(inPip()).toBeNull();
    expect(browser.exit).not.toHaveBeenCalled();
  });

  it('does nothing where the platform has no PiP (never on iOS in M1), though the browser has the call', () => {
    const browser = installPip();
    controller(false).enter();
    expect(browser.request).not.toHaveBeenCalled();
    expect(inPip()).toBeNull();
  });

  it('does nothing where the video has no such call, and on an empty stage', () => {
    const noApi = controller();
    noApi.enter(); // jsdom: no requestPictureInPicture
    expect(inPip()).toBeNull();
    detach?.();

    const browser = installPip();
    video.remove();
    controller().enter();
    expect(browser.request).not.toHaveBeenCalled();
  });

  it('changes nothing when the browser refuses', async () => {
    const browser = installPip();
    browser.request.mockImplementation(() => Promise.reject(new DOMException('no metadata', 'InvalidStateError')));
    controller().enter();
    await Promise.resolve();
    expect(inPip()).toBeNull();
  });

  it('closes the window when the share ends: the store drops it, and the browser follows', () => {
    const browser = installPip();
    controller().enter();
    syncRoom(viewer, room(shareInfo('s_bea', 'u_bea', 1)), SELF);
    expect(inPip()).toBeNull();
    expect(browser.exit).toHaveBeenCalledOnce();
    expect(browser.element).toBeNull();
  });

  it('closes the window when the page leaves its room, and when the stage goes away', () => {
    const browser = installPip();
    const pip = controller();
    pip.enter();
    viewer.store.getState().reset();
    expect(browser.exit).toHaveBeenCalledTimes(1);

    syncRoom(viewer, room(shareInfo('s_cy', 'u_cy', 2)), SELF);
    pip.enter();
    expect(inPip()).toBe('s_cy');
    detach?.();
    expect(browser.exit).toHaveBeenCalledTimes(2);
    expect(inPip()).toBeNull();
  });

  it('closes the window when the stage moves to another share: its video is a new element', () => {
    const browser = installPip();
    const pip = controller();
    pip.enter();
    pip.refresh(); // the same video
    expect(inPip()).toBe('s_cy');

    const next = document.createElement('video');
    video.replaceWith(next);
    stage.setAttribute('data-share-id', 's_bea');
    pip.refresh();
    expect(inPip()).toBeNull();
    expect(browser.exit).toHaveBeenCalledOnce();
  });

  it('ignores a window that shows some other video, and never closes it', () => {
    const browser = installPip();
    const other = document.createElement('video');
    document.body.append(other);
    const pip = controller();
    browser.element = other;
    other.dispatchEvent(new Event('enterpictureinpicture', { bubbles: true }));
    expect(inPip()).toBeNull();
    pip.exit();
    detach?.();
    expect(browser.exit).not.toHaveBeenCalled();
  });

  it('starts nothing before attach()', () => {
    const browser = installPip();
    const pip = createPip({ store: viewer.store, supported: true, target: () => ({ video, shareId: 's_cy' }) });
    pip.enter();
    expect(browser.request).not.toHaveBeenCalled();
  });
});
