import type { DirectoryController } from './controller.svelte';

export const personDirectoryContext = Symbol('person-directory');
export type PersonDirectoryGetter = () => DirectoryController;
