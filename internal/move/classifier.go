// Package move turns a Slack thread into a forum thread.
//
// This file defines how replies are sorted into direct responses to the
// original post, which are imported, and everything else, which stays in
// Slack.
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

// FakeClassifier marks exactly the IDs in Direct as direct; Err, when set, is returned.
// It is a deterministic stand-in for tests and is not safe for concurrent use.
type FakeClassifier struct {
	Direct map[string]bool
	Err    error
	// Calls counts Classify invocations.
	Calls int
}

// Classify implements Classifier.
func (f *FakeClassifier) Classify(_ context.Context, _ SlackMessage, replies []SlackMessage) (map[string]bool, error) {
	f.Calls++
	if f.Err != nil {
		return nil, f.Err
	}
	direct := make(map[string]bool)
	for _, r := range replies {
		if f.Direct[r.ID] {
			direct[r.ID] = true
		}
	}
	return direct, nil
}
