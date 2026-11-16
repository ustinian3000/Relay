package handoff

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	bolt "go.etcd.io/bbolt"
)

const continuation = "This session is being continued from a previous conversation"

func SummaryDigest(summary string) string {
	if i := strings.Index(summary, "<summary>"); i >= 0 {
		summary = summary[i+len("<summary>"):]
		if end := strings.Index(summary, "</summary>"); end >= 0 {
			summary = summary[:end]
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(summary), " ")))
	return hex.EncodeToString(sum[:])
}

// MatchEvidence accepts only the leading native continuation envelope. A receipt
// text embedded elsewhere in old conversation history is not adoption evidence.
func (e *Engine) MatchEvidence(scope, session string, body []byte) (string, bool, error) {
	var payload struct {
		Messages []struct {
			Role    string
			Content json.RawMessage
		}
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", false, err
	}
	opaque := false
	for _, m := range payload.Messages {
		var blocks []struct{ Type string }
		if json.Unmarshal(m.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "thinking" || b.Type == "redacted_thinking" || b.Type == "compaction" {
					opaque = true
				}
			}
		}
	}
	if len(payload.Messages) == 0 || payload.Messages[0].Role != "user" {
		return "", opaque, nil
	}
	var text string
	for _, message := range payload.Messages {
		if message.Role != "user" {
			return "", opaque, nil
		}
		var scalar string
		blocks := []struct{ Type, Text string }{}
		if json.Unmarshal(message.Content, &scalar) == nil {
			blocks = append(blocks, struct{ Type, Text string }{"text", scalar})
		} else if json.Unmarshal(message.Content, &blocks) != nil {
			return "", opaque, nil
		}
		for _, block := range blocks {
			if block.Type != "text" {
				return "", opaque, nil
			}
			if strings.HasPrefix(block.Text, continuation) {
				text = block.Text
				break
			}
			trimmed := strings.TrimSpace(block.Text)
			if !strings.HasPrefix(trimmed, "<system-reminder>") || !strings.HasSuffix(trimmed, "</system-reminder>") {
				return "", opaque, nil
			}
		}
		if text != "" {
			break
		}
	}
	if !strings.HasPrefix(text, continuation) {
		return "", opaque, nil
	}
	_, summary, ok := strings.Cut(text, "\nSummary:")
	if !ok {
		return "", opaque, nil
	}
	words := strings.Fields(summary)
	if len(words) > 32768 {
		return "", opaque, fault("summary_too_large")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	matched := ""
	err := e.db.View(func(tx *bolt.Tx) error {
		f, err := lookup(tx.Bucket(familyBucket), scope, session)
		if err != nil || f == nil {
			return err
		}
		expected := map[string]bool{}
		for _, d := range []string{f.NoticeSummary, f.ActiveSummary, f.MemberSummaries[session]} {
			if d != "" {
				expected[d] = true
			}
		}
		hash := sha256.New()
		for i, w := range words {
			if i > 0 {
				_, _ = hash.Write([]byte(" "))
			}
			_, _ = hash.Write([]byte(w))
			digest := hex.EncodeToString(hash.Sum(nil))
			if expected[digest] {
				matched = digest
				break
			}
		}
		return nil
	})
	return matched, opaque, err
}
