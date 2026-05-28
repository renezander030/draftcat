//go:build voice

package main

func init() {
	validKnownActions["voice_calls_completed"] = "harvest completed voice calls from Dograh writeback (deduped per pipeline)"
	validKnownActions["voice_handoffs_pending"] = "harvest unresolved handoff requests for human routing"
	validKnownActions["voice_handoffs_resolve"] = "resolve handoffs by writing ai_output.resolutions[].{handoff_id,target} back to the voice store"
	validKnownActions["voice_learnings_new"] = "harvest agent-flagged learning items for the 7-step review flow"
}
