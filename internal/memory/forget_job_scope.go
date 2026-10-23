package memory

// forgetJobScope identifies jobs before source rows are erased. Worldline jobs
// span conversations, so their profile snapshot ID cannot locate the sources.
// The three bound parameters are the affected conversation ID. Alias is an
// internal SQL identifier, never request input.
func forgetJobScope(alias string) string {
	return `(` + alias + `.input_snapshot_id IN (SELECT id FROM source_attachments WHERE conversation_id=?) OR ` + alias + `.input_snapshot_id=? OR (` + alias + `.stage='worldline' AND EXISTS (
	 SELECT 1 FROM json_each(CASE WHEN json_valid(` + alias + `.payload_json) THEN ` + alias + `.payload_json ELSE '{}' END,'$.references') ref
	 JOIN turns source_turn ON source_turn.id=json_extract(ref.value,'$.turn_id')
	 WHERE source_turn.conversation_id=?)))`
}
