// Vitest setup shared by every component spec: it registers jest-dom matchers
// (toBeInTheDocument, toHaveTextContent, …) and shims the browser APIs Mantine
// touches at render time but jsdom does not implement — matchMedia (used by the
// color-scheme manager) and ResizeObserver / scrollIntoView (used by overlays).
import "@testing-library/jest-dom/vitest";
import { afterEach, vi } from "vitest";
import { cleanup } from "@testing-library/react";

afterEach(() => cleanup());

Object.defineProperty(window, "matchMedia", {
  writable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: vi.fn(),
    removeListener: vi.fn(),
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
  }),
});

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
window.ResizeObserver = ResizeObserverStub;

if (!window.HTMLElement.prototype.scrollIntoView) {
  window.HTMLElement.prototype.scrollIntoView = () => {};
}

// Mantine's CopyButton uses navigator.clipboard, which jsdom does not provide.
// configurable so @testing-library/user-event can install its own stub on setup.
Object.defineProperty(navigator, "clipboard", {
  writable: true,
  configurable: true,
  value: { writeText: vi.fn().mockResolvedValue(undefined) },
});
