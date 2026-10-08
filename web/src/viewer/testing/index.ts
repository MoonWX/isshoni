// Wire fixtures for the viewer's tests: shares and participants as room.state has them (01 §8.5), and a fake
// IntersectionObserver for what is in view (05 §12.4). Only tests import this folder.
import { vi } from 'vitest';

import type { ParticipantInfo, ShareInfo, SubscriptionStatus } from '../../protocol/types.gen';
import type { RoomSnapshot, ViewerSelf } from '../viewerStore';

/** This page in the fixtures: Alex, on connection c_me. */
export const SELF: ViewerSelf = { userId: 'u_alex', connectionId: 'c_me' };

/** A share that started `minute` minutes into the hour; live, a window, nobody watching, unless overridden. */
export function shareInfo(id: string, userId: string, minute: number, overrides: Partial<ShareInfo> = {}): ShareInfo {
  return {
    id,
    userId,
    connectionId: `c_${userId}`,
    kind: 'window',
    preset: 'auto',
    audio: true,
    status: 'live',
    layers: ['high', 'low'],
    codec: 'h264/6400',
    startedAt: `2026-10-12T19:${String(minute).padStart(2, '0')}:00.000Z`,
    watchers: [],
    ...overrides,
  };
}

export function participant(userId: string, name: string): ParticipantInfo {
  return { userId, name, status: 'present', joinedAt: '2026-10-12T19:00:00.000Z', connections: [] };
}

/** A room.state with these shares; Alex (this page), Bea and Cy are in the room. */
export function room(...shares: ShareInfo[]): RoomSnapshot {
  return {
    shares,
    participants: [participant('u_alex', 'Alex'), participant('u_bea', 'Bea'), participant('u_cy', 'Cy')],
  };
}

export function status(shareId: string, overrides: Partial<SubscriptionStatus> = {}): SubscriptionStatus {
  return { shareId, video: 'high', audio: 'on', requestedVideo: 'high', ...overrides };
}

/** What a test does with the fake IntersectionObserver. */
export interface FakeIntersections {
  /** Reports to every observer of el how much of it is in the viewport (0–1), as the browser would. */
  show(el: Element, ratio: number): void;
  /** The elements that are observed now. */
  observed(): Element[];
}

/**
 * Puts a fake IntersectionObserver on globalThis (jsdom has none); vi.unstubAllGlobals() removes it. Observing an
 * element reports `initial` for it at once (a real browser does it a moment later); null reports nothing until the
 * test calls show().
 */
export function installFakeIntersectionObserver(initial: number | null = 1): FakeIntersections {
  const observers = new Set<FakeIntersectionObserver>();

  class FakeIntersectionObserver {
    readonly targets = new Set<Element>();
    readonly #callback: IntersectionObserverCallback;

    constructor(callback: IntersectionObserverCallback) {
      this.#callback = callback;
      observers.add(this);
    }

    observe(el: Element): void {
      this.targets.add(el);
      if (initial !== null) this.report(el, initial);
    }

    unobserve(el: Element): void {
      this.targets.delete(el);
    }

    disconnect(): void {
      this.targets.clear();
      observers.delete(this);
    }

    takeRecords(): IntersectionObserverEntry[] {
      return [];
    }

    report(el: Element, ratio: number): void {
      const entry = { target: el, isIntersecting: ratio > 0, intersectionRatio: ratio };
      this.#callback([entry as IntersectionObserverEntry], this as unknown as IntersectionObserver);
    }
  }

  vi.stubGlobal('IntersectionObserver', FakeIntersectionObserver);
  return {
    show(el, ratio) {
      for (const observer of [...observers]) {
        if (observer.targets.has(el)) observer.report(el, ratio);
      }
    },
    observed() {
      return [...new Set([...observers].flatMap((o) => [...o.targets]))];
    },
  };
}
