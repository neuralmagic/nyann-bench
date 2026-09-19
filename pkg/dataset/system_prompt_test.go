package dataset

import "testing"

func TestWithSystemPromptStampsEveryConversation(t *testing.T) {
	ds := &WithSystemPrompt{Inner: NewFaker(16, 4, 2, 4.0), Prompt: "shared prefix"}
	for i := 0; i < 3; i++ {
		conv := ds.NextConversation()
		if conv.System != "shared prefix" {
			t.Fatalf("conversation %d: System = %q", i, conv.System)
		}
		if len(conv.Turns) != 2 {
			t.Fatalf("conversation %d: expected the inner dataset's 2 turns, got %d", i, len(conv.Turns))
		}
	}
}
