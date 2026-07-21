import { beforeEach, describe, expect, it, vi } from 'vitest';

const pdf = vi.hoisted(() => ({
  getDocument: vi.fn(),
  workerOptions: { workerSrc: '' }
}));

vi.mock('pdfjs-dist', () => ({
  getDocument: pdf.getDocument,
  GlobalWorkerOptions: pdf.workerOptions,
  version: '5.4.296'
}));
vi.mock('pdfjs-dist/build/pdf.worker.min.mjs?url', () => ({ default: '/pdf.worker.min.mjs' }));

import { MAX_PDF_PAGES, renderPDF } from './FileViewer.browser.svelte';

function loadingTask(pages: Array<{ width: number; height: number }>) {
  const destroy = vi.fn(async () => undefined);
  const cleanup = vi.fn();
  const document = {
    numPages: pages.length,
    getPage: vi.fn(async (number: number) => ({
      getViewport: () => pages[number - 1]!,
      render: () => ({ cancel: vi.fn(), promise: Promise.resolve() }),
      getTextContent: async () => ({ items: [] }),
      cleanup
    }))
  };
  return { promise: Promise.resolve(document), destroy, cleanup };
}

describe('PDF preview resource bounds', () => {
  beforeEach(() => {
    pdf.getDocument.mockReset();
    vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue({} as CanvasRenderingContext2D);
  });

  it('destroys a document with too many pages exactly once', async () => {
    const task = loadingTask(Array.from({ length: MAX_PDF_PAGES + 1 }, () => ({ width: 10, height: 10 })));
    pdf.getDocument.mockReturnValue(task);
    const host = document.createElement('div');

    await expect(renderPDF(new TextEncoder().encode('%PDF-1.4'), host, new AbortController().signal))
      .rejects.toThrow(/page preview limit/i);
    expect(task.destroy).toHaveBeenCalledOnce();
    expect(host.childElementCount).toBe(0);
  });

  it('cleans a huge page and an aggregate overflow without retaining canvases', async () => {
    for (const pages of [
      [{ width: 4_001, height: 4_000 }],
      Array.from({ length: 5 }, () => ({ width: 3_500, height: 4_000 }))
    ]) {
      const task = loadingTask(pages);
      pdf.getDocument.mockReturnValue(task);
      const host = document.createElement('div');
      await expect(renderPDF(new TextEncoder().encode('%PDF-1.4'), host, new AbortController().signal))
        .rejects.toThrow(/canvas limit/i);
      expect(task.cleanup).toHaveBeenCalled();
      expect(task.destroy).toHaveBeenCalledOnce();
      expect(host.childElementCount).toBe(0);
    }
  });

  it('returns an idempotent handle that releases all rendered DOM', async () => {
    const task = loadingTask([{ width: 10, height: 10 }]);
    pdf.getDocument.mockReturnValue(task);
    const host = document.createElement('div');
    const handle = await renderPDF(new TextEncoder().encode('%PDF-1.4'), host, new AbortController().signal);
    expect(host.querySelectorAll('canvas')).toHaveLength(1);

    handle.destroy();
    handle.destroy();
    await vi.waitFor(() => expect(task.destroy).toHaveBeenCalledOnce());
    expect(host.childElementCount).toBe(0);
  });

  it('cleans a page acquired after aborting an in-flight getPage call', async () => {
    let resolvePage: ((page: ReturnType<typeof pageFixture>) => void) | undefined;
    const page = pageFixture();
    const task = {
      promise: Promise.resolve({
        numPages: 1,
        getPage: vi.fn(() => new Promise<ReturnType<typeof pageFixture>>((resolve) => { resolvePage = resolve; }))
      }),
      destroy: vi.fn(async () => undefined)
    };
    pdf.getDocument.mockReturnValue(task);
    const host = document.createElement('div');
    const controller = new AbortController();
    const pending = renderPDF(new TextEncoder().encode('%PDF-1.4'), host, controller.signal);
    await vi.waitFor(() => expect(task.promise).resolves.toBeDefined());
    controller.abort();
    resolvePage?.(page);

    await expect(pending).rejects.toMatchObject({ name: 'AbortError' });
    expect(page.cleanup).toHaveBeenCalledOnce();
    expect(host.childElementCount).toBe(0);
  });
});

function pageFixture() {
  return {
    getViewport: () => ({ width: 10, height: 10 }),
    render: () => ({ cancel: vi.fn(), promise: Promise.resolve() }),
    getTextContent: async () => ({ items: [] }),
    cleanup: vi.fn()
  };
}
