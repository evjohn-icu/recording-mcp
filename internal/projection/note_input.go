package projection

// NoteInput is an already source-validated note. It lets read-time consumers
// reuse projection's kind and proposed-state rules within their own snapshot.
// The caller must check every evidence turn's currency, scope and ambient state.
type NoteInput struct {
	ID, ConversationID, Kind, Summary string
	TopicIDs, EvidenceTurnIDs         []string
	PrimaryStartMS                    *int64
}

func ProjectNote(in NoteInput) (Object, bool) {
	return projectNote(noteRow{id: in.ID, conversationID: in.ConversationID, kind: Kind(in.Kind), summary: in.Summary, topicIDs: in.TopicIDs, evidence: in.EvidenceTurnIDs, primaryStartMS: in.PrimaryStartMS})
}
