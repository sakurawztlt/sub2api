package service

import "context"

type grokEncryptedContentStripRetriedContextKey struct{}

func grokEncryptedContentStripRetried(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	retried, _ := ctx.Value(grokEncryptedContentStripRetriedContextKey{}).(bool)
	return retried
}

func markGrokEncryptedContentStripRetried(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, grokEncryptedContentStripRetriedContextKey{}, true)
}
