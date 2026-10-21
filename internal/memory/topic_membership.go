package memory

// currentTopicMembershipSQL is shared by taxonomy lists and timeline filters.
// Historical topic_conversations rows remain available to audit SQL, but are
// not proof that a topic still has a current, non-ambient source.
func currentTopicMembershipSQL() string {
	return `SELECT tt.topic_id, t.conversation_id FROM turn_topics tt
	JOIN turns t ON t.id=tt.turn_id
	WHERE ` + CurrentTurnPredicate("t") + `
	AND NOT EXISTS(SELECT 1 FROM turn_classes tc WHERE tc.turn_id=t.id AND tc.class='ambient')
	UNION
	SELECT CAST(j.value AS TEXT) AS topic_id, n.conversation_id
	FROM notes n JOIN json_each(n.topic_ids_json) j
	WHERE NOT ` + NoteStalePredicate("n") + `
	AND NOT EXISTS(SELECT 1 FROM note_evidence ne JOIN turn_classes tc ON tc.turn_id=ne.turn_id
	WHERE ne.note_id=n.id AND ne.ordinal=0 AND tc.class='ambient')`
}
