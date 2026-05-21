// Package fake supplies deterministic llmtypes.Provider and llmtypes.Stream
// implementations for tests. Both types accept functional options so each
// test case can shape per-model and global behavior (errors, delays, canned
// events, summaries) without sharing fixture state across packages.
package fake
