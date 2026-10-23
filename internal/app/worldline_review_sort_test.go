package app

import "testing"

func TestWorldlineThreadsPrioritizeSemanticReview(t *testing.T) {
	threads := []WorldlineThread{
		{ID: "routine", ConversationCount: 6, SemanticReviewCount: 0},
		{ID: "needs-review", ConversationCount: 2, SemanticReviewCount: 1},
		{ID: "more-review", ConversationCount: 1, SemanticReviewCount: 2},
	}
	sortWorldlineThreads(threads)
	if threads[0].ID != "more-review" || threads[1].ID != "needs-review" || threads[2].ID != "routine" {
		t.Fatalf("threads are not ordered by review count before conversation count: %+v", threads)
	}
}

func TestFilterThreadBuildsCountsOnlyCurrentFilteredReviews(t *testing.T) {
	builds := []threadBuild{{
		thread: WorldlineThread{ID: "thread", Title: "Topic"},
		entries: []WorldlineThreadEntry{
			{ID: "review", Status: "unconfirmed", SemanticSuggestion: &WorldlineSemanticSuggestion{Status: "review_required", RequiresReview:true}},
			{ID: "uncertain", Status: "unconfirmed", SemanticSuggestion: &WorldlineSemanticSuggestion{Status: "insufficient_evidence"}},
			{ID: "routine", Status: "reviewed"},
		},
	}}
	filtered := filterThreadBuilds(builds, WorldlineQuery{Status: "unconfirmed"})
	if len(filtered) != 1 || filtered[0].thread.SemanticReviewCount != 1 || filtered[0].thread.EntryCount != 2 {
		t.Fatalf("review count did not follow filtered current suggestions: %+v", filtered)
	}
}
