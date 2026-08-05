package claude

import (
	"regexp"
	"sync"
	"testing"
)

func TestGenerateMessageIDMatchesAnthropicShape(t *testing.T) {
	pattern := regexp.MustCompile(`^msg_01[A-Za-z0-9]{22}$`)
	const count = 256
	ids := make(chan string, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- GenerateMessageID()
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]struct{}, count)
	for id := range ids {
		if !pattern.MatchString(id) {
			t.Fatalf("message ID does not match Anthropic shape: %q", id)
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate message ID generated: %q", id)
		}
		seen[id] = struct{}{}
	}
}
