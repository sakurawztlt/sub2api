package service

import (
	"context"
	"strings"
	"testing"
	"time"
)

type relayUpdateCacheStub struct{}

func (relayUpdateCacheStub) GetUpdateInfo(context.Context) (string, error) {
	return "", context.Canceled
}
func (relayUpdateCacheStub) SetUpdateInfo(context.Context, string, time.Duration) error { return nil }

type relayGitHubClientStub struct {
	fetchCalls int
}

func (s *relayGitHubClientStub) FetchLatestRelease(context.Context, string) (*GitHubRelease, error) {
	s.fetchCalls++
	return nil, context.Canceled
}

func (s *relayGitHubClientStub) DownloadFile(context.Context, string, string, int64) error {
	s.fetchCalls++
	return context.Canceled
}

func (s *relayGitHubClientStub) FetchChecksumFile(context.Context, string) ([]byte, error) {
	s.fetchCalls++
	return nil, context.Canceled
}

func TestUpdateServiceRelayBuildDisablesGitHubAndSelfUpdate(t *testing.T) {
	client := &relayGitHubClientStub{}
	svc := NewUpdateService(relayUpdateCacheStub{}, client, "0.1.115-relay", "release")

	info, err := svc.CheckUpdate(context.Background(), false)
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if info.CurrentVersion != "0.1.115-relay" || info.LatestVersion != "0.1.115-relay" {
		t.Fatalf("unexpected version info: %+v", info)
	}
	if info.HasUpdate {
		t.Fatalf("relay build must not report update available: %+v", info)
	}
	if !strings.Contains(info.Warning, "disabled") {
		t.Fatalf("expected disabled warning, got %+v", info)
	}
	if client.fetchCalls != 0 {
		t.Fatalf("relay build should not hit GitHub, fetchCalls=%d", client.fetchCalls)
	}

	if err := svc.PerformUpdate(context.Background()); err == nil {
		t.Fatal("PerformUpdate should be disabled for relay build")
	}
	if err := svc.Rollback(); err == nil {
		t.Fatal("Rollback should be disabled for relay build")
	}
	if client.fetchCalls != 0 {
		t.Fatalf("relay build update actions should not hit GitHub, fetchCalls=%d", client.fetchCalls)
	}
}
