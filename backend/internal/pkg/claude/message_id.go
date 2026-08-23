package claude

import (
	"crypto/rand"
	"math/big"
	"strings"
)

const (
	messageIDPrefix   = "msg_01"
	messageIDBodyLen  = 22
	messageIDAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

var messageIDAlphabetSize = big.NewInt(int64(len(messageIDAlphabet)))

// GenerateMessageID returns an Anthropic-shaped message ID. Keep this shape in
// sync with cc-api's generated IDs so every relay path emits the same
// mixed-case alphanumeric fingerprint.
func GenerateMessageID() string {
	var b strings.Builder
	b.Grow(len(messageIDPrefix) + messageIDBodyLen)
	_, _ = b.WriteString(messageIDPrefix)
	for i := 0; i < messageIDBodyLen; i++ {
		n, err := rand.Int(rand.Reader, messageIDAlphabetSize)
		if err != nil {
			// Entropy failure is practically unrecoverable, but a syntactically
			// valid ID is safer on the response hot path than a panic or a tell.
			_ = b.WriteByte(messageIDAlphabet[0])
			continue
		}
		_ = b.WriteByte(messageIDAlphabet[n.Int64()])
	}
	return b.String()
}
