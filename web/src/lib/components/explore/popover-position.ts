import { floatingPopoverStyle } from '@kenn-io/kit-ui';

export function popoverStyle(trigger: HTMLElement, panel: HTMLElement): string {
  return floatingPopoverStyle({
    trigger: trigger.getBoundingClientRect(),
    viewportWidth: window.innerWidth,
    viewportHeight: window.innerHeight,
    popoverWidth: panel.offsetWidth,
    popoverHeight: panel.offsetHeight,
    align: 'start',
  });
}
