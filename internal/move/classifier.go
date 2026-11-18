package move

import "context"

// SlackMessage is one message of a Slack thread, already converted for the model.
type SlackMessage struct {
	ID     string // the Slack ts; unique within the thread
	Author string // display name
	Text   string
}

// Classifier decides which replies answer the original post directly.
type Classifier interface {
	// Classify returns the set of reply IDs that are direct responses to original.
	// Every returned ID is one of replies[i].ID. A reply the classifier is unsure
	// about, or omits, is NOT direct (it stays in Slack — plan R17, KTD8).
	//
	// The map holds only direct replies, each with the value true, and is
	// non-nil whenever the error is nil.
	Classify(ctx context.Context, original SlackMessage, replies []SlackMessage) (map[string]bool, error)
}
