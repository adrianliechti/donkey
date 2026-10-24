// The Source tab's place in the URL: nothing for the history, `commit` for
// one commit, `files` (and `path`) for its files.
export type SourceSearch = { commit?: string; files?: 1; path?: string }
