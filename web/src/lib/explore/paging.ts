// Each walk-to-end invocation loads at most this many pages so `End` on a
// multi-million-row view cannot spiral into thousands of sequential
// round-trips. The cursor is preserved: pressing End again continues.
export const LOAD_THROUGH_END_MAX_PAGES = 20;
