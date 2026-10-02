import brand from '@kenn-io/kit-ui/brand.json';

/** Thickness of kit-ui's SplitResizeHandle in CSS pixels. kit-ui owns the
 * value (`layout.splitHandleSize`, rendered as `--split-handle-size`), so
 * split panes reserve exactly this much room for the handle. */
export const splitHandleSize = parseFloat(brand.layout.splitHandleSize);
