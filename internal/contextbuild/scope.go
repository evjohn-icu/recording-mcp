package contextbuild

import (
	"context"
	"database/sql"
	"time"

	"recordingmcp/internal/memory"
)

// requestScope holds the user's explicit hard filters. Every assembly branch
// must satisfy them: a result only enters the package when it matches the
// requested query AND the requested conversation AND at least one requested
// topic AND the requested time bounds. Scope is never "extra content" — it is
// a constraint applied before anything is added, with early query-level
// narrowing where the store supports it.
//
// Time semantics mirror the retrieval layer: bounds only apply to
// conversations whose recording start is known, and a bounded request
// excludes conversations with unknown start times.
type requestScope struct {
	conversationID string
	topicIDs       map[string]bool // any-of; empty means unconstrained
	after          *time.Time
	before         *time.Time

	// store serves the shared conversation-start lookup.
	store *memory.Store

	// startedAt memoizes conversation -> recording start so time scoping
	// costs one query per covered conversation.
	startedAt map[string]*time.Time
}

func newRequestScope(store *memory.Store, convID string, topicIDs []string, after, before *time.Time) *requestScope {
	set := map[string]bool{}
	for _, tid := range topicIDs {
		set[tid] = true
	}
	if len(set) == 0 {
		set = nil
	}
	return &requestScope{
		conversationID: convID,
		topicIDs:       set,
		after:          after,
		before:         before,
		store:          store,
		startedAt:      map[string]*time.Time{},
	}
}

// hasTimeBounds reports whether the request constrains time at all.
func (s *requestScope) hasTimeBounds() bool { return s.after != nil || s.before != nil }

// topicScoped reports whether the request pins the topic scope.
func (s *requestScope) topicScoped() bool { return len(s.topicIDs) > 0 }

// topicAllowed reports whether a topic id is inside the requested topic scope.
func (s *requestScope) topicAllowed(tid string) bool {
	if !s.topicScoped() {
		return true
	}
	return s.topicIDs[tid]
}

// conversationAllowed reports whether a conversation id is inside the
// requested conversation scope.
func (s *requestScope) conversationAllowed(cid string) bool {
	return s.conversationID == "" || cid == s.conversationID
}

// allows reports whether one item satisfies every explicit scope constraint.
// Items are checked before they enter the candidate set regardless of the
// branch that produced them, so no branch can leak out-of-scope content.
func (s *requestScope) allows(ctx context.Context, db *sql.DB, item memory.Item) (bool, error) {
	if !s.conversationAllowed(item.ConversationID) {
		return false, nil
	}
	if s.topicScoped() {
		inScope := false
		for _, tid := range item.TopicIDs {
			if s.topicIDs[tid] {
				inScope = true
				break
			}
		}
		if !inScope {
			return false, nil
		}
	}
	return s.timeAllows(ctx, db, item.ConversationID)
}

// timeAllows applies the time bounds to the item's conversation using the
// same semantics as the retrieval layer: unknown recording start is excluded
// whenever a bound is set.
func (s *requestScope) timeAllows(ctx context.Context, db *sql.DB, conversationID string) (bool, error) {
	if !s.hasTimeBounds() {
		return true, nil
	}
	started, err := s.conversationStartedAt(ctx, db, conversationID)
	if err != nil {
		return false, err
	}
	return memory.WithinStartedRange(started, s.after, s.before), nil
}

// conversationStartedAt reads (and memoizes) the recording start of one
// conversation through the store's shared lookup.
func (s *requestScope) conversationStartedAt(ctx context.Context, db *sql.DB, conversationID string) (*time.Time, error) {
	if started, ok := s.startedAt[conversationID]; ok {
		return started, nil
	}
	started, err := s.store.ConversationStartedAt(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	s.startedAt[conversationID] = started
	return started, nil
}
