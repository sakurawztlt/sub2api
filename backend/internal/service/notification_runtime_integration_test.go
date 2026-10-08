//go:build unit

package service

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"mime/quotedprintable"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newNotificationRuntimeFixture(t *testing.T) (*notificationEmailMemorySettingRepo, *EmailService, *NotificationEmailService, *notificationEmailTestSMTPServer) {
	t.Helper()
	repo := newNotificationEmailMemorySettingRepo()
	smtp := startNotificationEmailTestSMTPServer(t)
	require.NoError(t, repo.SetMultiple(context.Background(), smtp.settings()))
	require.NoError(t, repo.Set(context.Background(), SettingKeyAPIBaseURL, "https://relay.example"))
	email := NewEmailService(repo, nil)
	notifications := NewNotificationEmailService(repo, email)
	email.SetNotificationEmailService(notifications)
	return repo, email, notifications, smtp
}

func notificationRuntimeHTML(t *testing.T, raw string) string {
	t.Helper()
	_, body, ok := strings.Cut(raw, "\r\n\r\n")
	require.True(t, ok)
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
	require.NoError(t, err)
	return string(decoded)
}

func TestNotificationRuntimeBalanceTemplateUnsubscribeAndDeduplication(t *testing.T) {
	ctx := context.Background()
	repo, email, notifications, smtp := newNotificationRuntimeFixture(t)
	_, err := notifications.UpdateTemplate(ctx, NotificationEmailEventBalanceLow, "en", "BALANCE-CUSTOM", `<p>BALANCE-CUSTOM {{current_balance}} {{threshold}}</p><a href="{{unsubscribe_url}}">unsubscribe</a>`)
	require.NoError(t, err)
	require.NoError(t, repo.Set(ctx, notificationEmailPreferenceKey(NotificationEmailEventBalanceLow, "muted@example.com"), "unsubscribed"))
	balance := NewBalanceNotifyService(email, repo, nil)
	balance.SetNotificationEmailService(notifications)
	for range 2 {
		balance.sendBalanceLowEmails([]string{"muted@example.com", "user@example.com"}, 42, "User", "user@example.com", 7.25, 10, "Relay", "https://relay.example/recharge")
	}
	require.Equal(t, int64(1), smtp.messageCount(), "unsubscribed recipients and duplicate reminders must not fall back to the built-in email")
	body := notificationRuntimeHTML(t, smtp.messageBodies()[0])
	require.Contains(t, body, "BALANCE-CUSTOM")
	require.Contains(t, body, "7.25")
	require.Contains(t, body, "/api/v1/settings/email-unsubscribe?token")
	require.NotContains(t, body, "Balance Low Alert")
	link := regexp.MustCompile(`href="([^"]+)"`).FindStringSubmatch(body)
	require.Len(t, link, 2)
	u, err := url.Parse(html.UnescapeString(link[1]))
	require.NoError(t, err)
	token := u.Query().Get("token")
	require.NotEmpty(t, token)
	_, err = notifications.Unsubscribe(ctx, token+"tampered")
	require.Error(t, err)
	result, err := notifications.Unsubscribe(ctx, token)
	require.NoError(t, err)
	require.Equal(t, "user@example.com", result.Email)
	require.Equal(t, NotificationEmailEventBalanceLow, result.Event)
	_, err = repo.GetValue(ctx, notificationEmailDeliveryKey(NotificationEmailEventBalanceLow, "balance_low", strconv.Itoa(42), "user@example.com", time.Now().UTC().Format("2006-01-02")))
	require.NoError(t, err)
}

func TestNotificationRuntimeScheduledReportTemplateAndDeduplication(t *testing.T) {
	ctx := context.Background()
	repo, email, notifications, smtp := newNotificationRuntimeFixture(t)
	_, err := notifications.UpdateTemplate(ctx, NotificationEmailEventOpsScheduledReport, "en", "REPORT-CUSTOM", `<p>REPORT-CUSTOM {{report_total_requests}}</p>`)
	require.NoError(t, err)
	ops := &OpsService{opsRepo: &opsRepoMock{}, settingRepo: repo}
	reports := NewOpsScheduledReportService(ops, nil, email, nil, nil)
	report := &opsScheduledReport{Name: "Daily", ReportType: "daily_summary", TimeRange: 24 * time.Hour, Recipients: []string{"ops@example.com"}}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for range 2 {
		attempts, err := reports.runReport(ctx, report, now)
		require.NoError(t, err)
		require.Equal(t, 1, attempts)
	}
	require.Equal(t, int64(1), smtp.messageCount())
	require.Contains(t, smtp.messageBodies()[0], "REPORT-CUSTOM")
	require.Contains(t, smtp.messageBodies()[0], "0")
}

type notificationRuntimeOpsRepo struct {
	opsRepoMock
	marked []int64
}

func (r *notificationRuntimeOpsRepo) UpdateAlertEventEmailSent(_ context.Context, id int64, sent bool) error {
	if sent {
		r.marked = append(r.marked, id)
	}
	return nil
}

func TestNotificationRuntimeOpsAlertTemplateRetainsSilencingAndDeduplication(t *testing.T) {
	ctx := context.Background()
	repo, email, notifications, smtp := newNotificationRuntimeFixture(t)
	_, err := notifications.UpdateTemplate(ctx, NotificationEmailEventOpsAlert, "en", "ALERT-CUSTOM", `<p>ALERT-CUSTOM {{rule_name}} {{metric_value}}</p>`)
	require.NoError(t, err)
	emailCfg := OpsEmailNotificationConfig{Alert: OpsEmailAlertConfig{Enabled: true, Recipients: []string{"ops@example.com"}, RateLimitPerHour: 20}}
	raw, err := json.Marshal(emailCfg)
	require.NoError(t, err)
	require.NoError(t, repo.Set(ctx, SettingKeyOpsEmailNotificationConfig, string(raw)))
	opsRepo := &notificationRuntimeOpsRepo{}
	ops := &OpsService{opsRepo: opsRepo, settingRepo: repo}
	alerts := NewOpsAlertEvaluatorService(ops, opsRepo, email, nil, nil, nil)
	rule := &OpsAlertRule{Name: "rule-canary", NotifyEmail: true, Severity: "critical"}
	event := &OpsAlertEvent{ID: 91}
	quiet := &OpsAlertRuntimeSettings{Silencing: OpsAlertSilencingSettings{Enabled: true, GlobalUntilRFC3339: time.Now().Add(time.Hour).Format(time.RFC3339)}}
	require.False(t, alerts.maybeSendAlertEmail(ctx, quiet, rule, event))
	require.Zero(t, smtp.messageCount())
	require.Empty(t, opsRepo.marked)
	require.True(t, alerts.maybeSendAlertEmail(ctx, nil, rule, event))
	require.True(t, alerts.maybeSendAlertEmail(ctx, nil, rule, event))
	require.Equal(t, int64(1), smtp.messageCount())
	require.Contains(t, smtp.messageBodies()[0], "ALERT-CUSTOM")
	require.Contains(t, smtp.messageBodies()[0], "rule-canary")
	require.Equal(t, []int64{91, 91}, opsRepo.marked)
}
