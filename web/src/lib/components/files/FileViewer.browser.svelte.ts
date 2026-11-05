import { GlobalWorkerOptions, TextLayer, getDocument, version } from 'pdfjs-dist';
import 'pdfjs-dist/web/pdf_viewer.css';
import workerURL from 'pdfjs-dist/build/pdf.worker.min.mjs?url';

export const PDFJS_PINNED_VERSION = '5.4.296';
export const MAX_PDF_BYTES = 25 * 1024 * 1024;
export const MAX_PDF_PAGES = 40;
const MAX_CANVAS_PIXELS = 16_000_000;
export const MAX_TOTAL_CANVAS_PIXELS = 64_000_000;

export interface PDFRenderHandle {
  pages: number;
  setZoom(zoom: number): void;
  destroy(): void;
}

GlobalWorkerOptions.workerSrc = workerURL;

export async function renderPDF(
  bytes: Uint8Array,
  host: HTMLElement,
  signal: AbortSignal
): Promise<PDFRenderHandle> {
  if (version !== PDFJS_PINNED_VERSION) {
    throw new Error(`PDF renderer version mismatch: expected ${PDFJS_PINNED_VERSION}, got ${version}`);
  }
  if (bytes.byteLength > MAX_PDF_BYTES) throw new Error('This file is too large to preview.');
  if (signal.aborted) throw new DOMException('Preview cancelled', 'AbortError');

  const loadingTask = getDocument({ data: bytes });
  const pages = host.ownerDocument.createElement('div');
  pages.className = 'pdf-pages';
  const availableWidth = (): number => {
    const style = host.ownerDocument.defaultView!.getComputedStyle(pages);
    return host.clientWidth - (parseFloat(style.paddingLeft) || 0) - (parseFloat(style.paddingRight) || 0) - 24;
  };
  let activeRender: { cancel(): void; promise: Promise<unknown> } | undefined;
  let activePage: { cleanup(): void } | undefined;
  let destroyed = false;
  let zoom = 1;
  const layers: Array<{ section: HTMLElement; layer: TextLayer; page: Awaited<ReturnType<Awaited<typeof loadingTask.promise>['getPage']>> }> = [];
  const resize = (): void => {
    for (const { section, layer, page } of layers) {
      const natural = page.getViewport({ scale: 1 });
      const width = Math.min(availableWidth(), natural.width * 1.25) * zoom;
      const scale = Math.max(1, width) / natural.width;
      const viewport = page.getViewport({ scale });
      section.style.width = `${viewport.width}px`;
      section.style.height = `${viewport.height}px`;
      section.style.setProperty('--total-scale-factor', String(scale));
      layer.update({ viewport });
    }
  };
  const observer = new ResizeObserver(resize);
  observer.observe(host);
  const cleanup = async (): Promise<void> => {
    if (destroyed) return;
    destroyed = true;
    signal.removeEventListener('abort', abortLoading);
    observer.disconnect();
    for (const { layer } of layers) layer.cancel();
    activeRender?.cancel();
    activeRender = undefined;
    activePage?.cleanup();
    activePage = undefined;
    host.replaceChildren();
    await loadingTask.destroy();
  };
  const abortLoading = (): void => { void cleanup(); };
  signal.addEventListener('abort', abortLoading, { once: true });
  try {
    const pdfDocument = await loadingTask.promise;
    if (signal.aborted) throw new DOMException('Preview cancelled', 'AbortError');
    if (pdfDocument.numPages > MAX_PDF_PAGES) {
      throw new Error(`PDF exceeds the ${MAX_PDF_PAGES}-page preview limit.`);
    }

    host.replaceChildren(pages);
    let totalCanvasPixels = 0;
    for (let pageNumber = 1; pageNumber <= pdfDocument.numPages; pageNumber += 1) {
      if (signal.aborted) throw new DOMException('Preview cancelled', 'AbortError');
      const page = await pdfDocument.getPage(pageNumber);
      if (destroyed || signal.aborted) {
        page.cleanup();
        throw new DOMException('Preview cancelled', 'AbortError');
      }
      activePage = page;
      const baseViewport = page.getViewport({ scale: 1.25 });
      const basePixels = Math.ceil(baseViewport.width) * Math.ceil(baseViewport.height);
      if (basePixels > MAX_CANVAS_PIXELS) throw new Error(`PDF page ${pageNumber} exceeds the preview canvas limit.`);
      const desiredScale = Math.max(1.25, availableWidth() * 2 * (host.ownerDocument.defaultView?.devicePixelRatio ?? 1) / page.getViewport({ scale: 1 }).width);
      const budget = Math.min(MAX_CANVAS_PIXELS, (MAX_TOTAL_CANVAS_PIXELS - totalCanvasPixels) / (pdfDocument.numPages - pageNumber + 1));
      const scale = Math.max(1.25, Math.min(desiredScale, 1.25 * Math.sqrt(budget / basePixels)));
      const viewport = page.getViewport({ scale });
      // Round down before budgeting and allocating. Rounding both sides up
      // can exceed the limit even when the continuous area fits exactly.
      const width = Math.floor(viewport.width);
      const height = Math.floor(viewport.height);
      const pagePixels = width * height;
      if (pagePixels > MAX_CANVAS_PIXELS) {
        throw new Error(`PDF page ${pageNumber} exceeds the preview canvas limit.`);
      }
      totalCanvasPixels += pagePixels;
      if (totalCanvasPixels > MAX_TOTAL_CANVAS_PIXELS) {
        throw new Error('PDF exceeds the aggregate preview canvas limit.');
      }

      const section = host.ownerDocument.createElement('section');
      section.className = 'pdf-page';
      const canvas = host.ownerDocument.createElement('canvas');
      canvas.className = 'pdf-canvas';
      canvas.width = width;
      canvas.height = height;
      canvas.setAttribute('aria-label', `PDF page ${pageNumber}`);
      const context = canvas.getContext('2d');
      if (!context) throw new Error('Canvas rendering is unavailable.');
      section.append(canvas);

      activeRender = page.render({ canvas, canvasContext: context, viewport });
      await activeRender.promise;
      activeRender = undefined;
      if (destroyed || signal.aborted) throw new DOMException('Preview cancelled', 'AbortError');

      const text = await page.getTextContent();
      if (destroyed || signal.aborted) throw new DOMException('Preview cancelled', 'AbortError');
      const textLayer = host.ownerDocument.createElement('div');
      textLayer.className = 'textLayer';
      textLayer.setAttribute('aria-label', `Text from PDF page ${pageNumber}`);
      const layer = new TextLayer({ textContentSource: text, container: textLayer, viewport: page.getViewport({ scale: 1.25 }) });
      section.style.setProperty('--total-scale-factor', '1.25');
      section.append(textLayer);
      layers.push({ section, layer, page });
      resize();
      pages.append(section);
      await layer.render();
      if (destroyed || signal.aborted) throw new DOMException('Preview cancelled', 'AbortError');
      page.cleanup();
      activePage = undefined;
    }
    return {
      pages: pdfDocument.numPages,
      setZoom(value: number): void { zoom = value; resize(); },
      destroy(): void { void cleanup(); }
    };
  } catch (error) {
    await cleanup();
    throw error;
  }
}
