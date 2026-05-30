//go:build voice

package validate

func init() {
	validKnownActions["voice_calls_completed"] = "harvest completed voice calls from Dograh writeback (deduped per pipeline)"
	validKnownActions["voice_handoffs_pending"] = "harvest unresolved handoff requests for human routing"
	validKnownActions["voice_handoffs_resolve"] = "resolve handoffs by writing ai_output.resolutions[].{handoff_id,target} back to the voice store"
	validKnownActions["voice_learnings_new"] = "harvest agent-flagged learning items for the 7-step review flow"
	validKnownActions["git_commit_workflow_update"] = "stage + commit a workflow definition file (vars: path, content_var?, message?/message_var?, repo_dir?)"
	validKnownActions["dograh_staging_smoke"] = "trigger an outbound run on Dograh staging to smoke-test a new workflow version (vars: workflow_uuid_var?, initial_context_var?)"
	validKnownActions["dograh_prod_publish"] = "PUT an updated workflow_definition to Dograh prod (auto-versions); vars: workflow_id_var?, definition_path"
}
