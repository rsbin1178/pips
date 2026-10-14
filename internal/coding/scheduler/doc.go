// Package scheduler owns the durable per-session scheduled-task list and the
// interval contract the model-facing scheduler Tools share.
//
// The store follows the plan store's private-file discipline: a private
// subdirectory of the session's own state directory, mode 0700 with a 0600
// file, an atomic replace, a bounded size, and a versioned schema. The firing
// loop belongs to the Coding Runtime; this package owns only the record, the
// interval, and the store.
package scheduler
