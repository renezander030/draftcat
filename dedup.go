package main

import "log"

// dedupByID is a generic helper for fetch actions: extract IDs, filter to
// unseen, mark all fetched IDs as seen. Returns the filtered slice. Failures
// in the state layer log a warning but never block the pipeline — dedup
// degrades to "process everything" rather than silently dropping items.
func dedupByID[T any](pipeline, scope string, items []T, keyFn func(T) string) []T {
	if state == nil || len(items) == 0 {
		return items
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = keyFn(it)
	}
	unseen, err := state.FilterUnseen(pipeline, scope, ids)
	if err != nil {
		log.Printf("[state] FilterUnseen(%s/%s) failed (proceeding without dedup): %v", pipeline, scope, err)
		return items
	}
	unseenSet := make(map[string]struct{}, len(unseen))
	for _, id := range unseen {
		unseenSet[id] = struct{}{}
	}
	out := make([]T, 0, len(unseen))
	for i, it := range items {
		if _, ok := unseenSet[ids[i]]; ok {
			out = append(out, it)
		}
	}
	if err := state.MarkSeen(pipeline, scope, ids); err != nil {
		log.Printf("[state] MarkSeen(%s/%s) failed: %v", pipeline, scope, err)
	}
	return out
}
