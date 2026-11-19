package move

import "context"

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
