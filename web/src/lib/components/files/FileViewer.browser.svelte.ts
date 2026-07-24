import { GlobalWorkerOptions, getDocument, version } from 'pdfjs-dist';
import workerURL from 'pdfjs-dist/build/pdf.worker.min.mjs?url';

export const PDFJS_PINNED_VERSION = '5.4.296';
export const MAX_PDF_BYTES = 25 * 1024 * 1024;
export const MAX_PDF_PAGES = 40;
const MAX_CANVAS_PIXELS = 16_000_000;
export const MAX_TOTAL_CANVAS_PIXELS = 64_000_000;

export interface PDFRenderHandle {
  pages: number;
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
  if (bytes.byteLength > MAX_PDF_BYTES) throw new Error('PDF exceeds the preview byte limit.');
  if (signal.aborted) throw new DOMException('Preview cancelled', 'AbortError');

  const loadingTask = getDocument({ data: bytes });
  let activeRender: { cancel(): void; promise: Promise<unknown> } | undefined;
  let activePage: { cleanup(): void } | undefined;
  let destroyed = false;
  const cleanup = async (): Promise<void> => {
    if (destroyed) return;
    destroyed = true;
    signal.removeEventListener('abort', abortLoading);
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

    host.replaceChildren();
    let totalCanvasPixels = 0;
    for (let pageNumber = 1; pageNumber <= pdfDocument.numPages; pageNumber += 1) {
      if (signal.aborted) throw new DOMException('Preview cancelled', 'AbortError');
      const page = await pdfDocument.getPage(pageNumber);
      if (destroyed || signal.aborted) {
        page.cleanup();
        throw new DOMException('Preview cancelled', 'AbortError');
      }
      activePage = page;
      const viewport = page.getViewport({ scale: 1.25 });
      const pagePixels = Math.ceil(viewport.width) * Math.ceil(viewport.height);
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
      canvas.width = Math.ceil(viewport.width);
      canvas.height = Math.ceil(viewport.height);
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
      textLayer.className = 'pdf-text';
      textLayer.setAttribute('aria-label', `Text from PDF page ${pageNumber}`);
      textLayer.textContent = text.items
        .map((item) => ('str' in item ? item.str : ''))
        .filter(Boolean)
        .join(' ');
      section.append(textLayer);
      host.append(section);
      page.cleanup();
      activePage = undefined;
    }
    return {
      pages: pdfDocument.numPages,
      destroy(): void { void cleanup(); }
    };
  } catch (error) {
    await cleanup();
    throw error;
  }
}
