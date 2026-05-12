// Package service - 2026-05-11 codex audit / R29 backup-only traffic capture.
//
// 完整留存 sub2api 三份 body: inbound (gcr 给的) / outbound (sub2api 改完发给上游的) /
// response (上游回的). 默认关闭, env-gated, 含 PII/api_key/signature, backup 验证后立刻关.
//
// 跟 ops_error_logger 区别:
//   - ops_error_logger: 200 跳过, 错误才记
//   - TrafficCapture: 200 也全量记, 整链路三份 body 完整, backup cctest 调试用
package service

import (
	"context"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/trafficcapture"
)

// TrafficCaptureConfig — env-driven, 默认全 OFF.
//
//	SUB2API_TRAFFIC_CAPTURE_ENABLED         = "true" / "1" 开
//	SUB2API_TRAFFIC_CAPTURE_MAX_BYTES       默认 262144 (256KB), backup 调试可 5MB
//	SUB2API_TRAFFIC_CAPTURE_TTL_HOURS       默认 24, janitor 自动清旧
//	SUB2API_TRAFFIC_CAPTURE_FILTER_API_KEY  逗号分隔 API key ID 白名单 (空=全部)
//	SUB2API_TRAFFIC_CAPTURE_FILTER_ACCOUNT  逗号分隔 account ID 白名单
//	SUB2API_TRAFFIC_CAPTURE_SAMPLING        采样 0.0-1.0, 默认 1.0 (100%)
type TrafficCaptureConfig struct {
	Enabled         bool
	MaxBytes        int
	TTL             time.Duration
	APIKeyFilter    map[int64]bool // 空 = 全部放过
	AccountFilter   map[int64]bool
	Sampling        float64 // 0.0-1.0
}

func LoadTrafficCaptureConfig() TrafficCaptureConfig {
	c := TrafficCaptureConfig{
		MaxBytes: 256 * 1024,
		TTL:      24 * time.Hour,
		Sampling: 1.0,
	}
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SUB2API_TRAFFIC_CAPTURE_ENABLED")))
	c.Enabled = v == "1" || v == "true" || v == "yes" || v == "on"
	if !c.Enabled {
		return c
	}
	if mb := strings.TrimSpace(os.Getenv("SUB2API_TRAFFIC_CAPTURE_MAX_BYTES")); mb != "" {
		if n, err := strconv.Atoi(mb); err == nil && n > 0 {
			c.MaxBytes = n
		}
	}
	if ttl := strings.TrimSpace(os.Getenv("SUB2API_TRAFFIC_CAPTURE_TTL_HOURS")); ttl != "" {
		if n, err := strconv.Atoi(ttl); err == nil && n > 0 {
			c.TTL = time.Duration(n) * time.Hour
		}
	}
	if f := strings.TrimSpace(os.Getenv("SUB2API_TRAFFIC_CAPTURE_FILTER_API_KEY")); f != "" {
		c.APIKeyFilter = parseInt64CSVSet(f)
	}
	if f := strings.TrimSpace(os.Getenv("SUB2API_TRAFFIC_CAPTURE_FILTER_ACCOUNT")); f != "" {
		c.AccountFilter = parseInt64CSVSet(f)
	}
	if s := strings.TrimSpace(os.Getenv("SUB2API_TRAFFIC_CAPTURE_SAMPLING")); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil && f >= 0 && f <= 1 {
			c.Sampling = f
		}
	}
	return c
}

func parseInt64CSVSet(s string) map[int64]bool {
	out := map[int64]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if n, err := strconv.ParseInt(p, 10, 64); err == nil {
			out[n] = true
		}
	}
	return out
}

// TrafficCaptureEntry — capture 一份请求 + 响应的完整快照.
//
// 2026-05-12 P5 fix: ResponseTotalBytes 记录响应原始总大小, 让 service.persist
// 准确判断 truncated (ResponseBody 已被 middleware writer cap-clamp 过).
type TrafficCaptureEntry struct {
	Ts                 time.Time
	RequestID          string
	UpstreamRequestID  string
	APIKeyID           int64
	AccountID          int64
	GroupID            int64
	Platform           string
	AccountType        string
	Model              string
	UpstreamStatus     int
	Stream             bool
	UseTimeMS          int64
	InboundBody        []byte
	OutboundBody       []byte
	ResponseBody       []byte
	ResponseTotalBytes int64 // P5: middleware writer 累积的真实总字节 (ResponseBody 可能被 cap 截了, 这个永远准)
	OutboundHeaders    map[string]string
	ResponseHeaders    map[string]string
	ErrorKind          string
	ErrorMsg           string
	ClientIP           string
	UserAgent          string
}

// TrafficCaptureService — 异步队列 + 截断 + 脱敏 + TTL 清理.
type TrafficCaptureService struct {
	client  *dbent.Client
	cfg     TrafficCaptureConfig
	queue   chan TrafficCaptureEntry
	dropped int64 // 队列满 drop 计数 (atomic)
	written int64
	failed  int64
	stopped int32         // atomic
	done    chan struct{} // P-A: signal consumeLoop/cleanupLoop exit
	doneAck sync.WaitGroup
}

// NewTrafficCaptureService — 跟 ent.Client + env config 一起初始化.
// 内部 goroutine 异步消费队列写盘, 失败 log 不阻塞 hot path.
// queueSize 控制内存上限 — 满了 drop 累计 dropped.
func NewTrafficCaptureService(client *dbent.Client, cfg TrafficCaptureConfig) *TrafficCaptureService {
	const queueSize = 256
	s := &TrafficCaptureService{
		client: client,
		cfg:    cfg,
		queue:  make(chan TrafficCaptureEntry, queueSize),
		done:   make(chan struct{}),
	}
	if cfg.Enabled {
		s.doneAck.Add(2)
		go s.consumeLoop()
		go s.cleanupLoop()
		log.Printf("[traffic-capture] enabled, max_bytes=%d ttl=%s sampling=%.2f api_key_filter=%v account_filter=%v",
			cfg.MaxBytes, cfg.TTL, cfg.Sampling, len(cfg.APIKeyFilter), len(cfg.AccountFilter))
	}
	return s
}

// Close — P-A: graceful shutdown. drain queue 已收到的 entry, 关 goroutine.
// sub2api shutdown 时调一次. 多调 idempotent. ctx 控制最长 drain 等待.
func (s *TrafficCaptureService) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if !atomic.CompareAndSwapInt32(&s.stopped, 0, 1) {
		return nil // 已 close
	}
	close(s.queue)
	close(s.done)
	wait := make(chan struct{})
	go func() { s.doneAck.Wait(); close(wait) }()
	select {
	case <-wait:
		log.Printf("[traffic-capture] closed cleanly (written=%d dropped=%d failed=%d)",
			atomic.LoadInt64(&s.written), atomic.LoadInt64(&s.dropped), atomic.LoadInt64(&s.failed))
	case <-ctx.Done():
		log.Printf("[traffic-capture] close timed out, %d entries may be lost", len(s.queue))
		return ctx.Err()
	}
	return nil
}

func (s *TrafficCaptureService) Enabled() bool {
	return s != nil && s.cfg.Enabled && atomic.LoadInt32(&s.stopped) == 0
}

// MaxBytesCap — middleware 读 inbound 时用同一 cap, 防 OOM 大请求.
func (s *TrafficCaptureService) MaxBytesCap() int {
	if s == nil || s.cfg.MaxBytes <= 0 {
		return 256 * 1024
	}
	return s.cfg.MaxBytes
}

// Submit — 非阻塞投递; 队列满直接 drop + atomic 计数. hot path 用.
// 调用方传 entry, 由 service 内部完成截断/脱敏 (跟 hot path 解耦防延迟).
func (s *TrafficCaptureService) Submit(entry TrafficCaptureEntry) {
	if !s.Enabled() {
		return
	}
	// filters
	if len(s.cfg.APIKeyFilter) > 0 && !s.cfg.APIKeyFilter[entry.APIKeyID] {
		return
	}
	if len(s.cfg.AccountFilter) > 0 && !s.cfg.AccountFilter[entry.AccountID] {
		return
	}
	// sampling
	if s.cfg.Sampling < 1.0 {
		// 用 request_id hash 做稳定采样, 同 reqID 同结果便于诊断
		if !samplingHit(entry.RequestID, s.cfg.Sampling) {
			return
		}
	}
	select {
	case s.queue <- entry:
	default:
		atomic.AddInt64(&s.dropped, 1)
	}
}

func samplingHit(reqID string, rate float64) bool {
	if rate >= 1.0 {
		return true
	}
	if rate <= 0.0 {
		return false
	}
	// 简单稳定 hash: sum bytes
	var sum uint32
	for i := 0; i < len(reqID); i++ {
		sum = sum*31 + uint32(reqID[i])
	}
	return float64(sum%10000)/10000.0 < rate
}

// Stats — 给 admin endpoint 暴露 dropped/written/failed.
func (s *TrafficCaptureService) Stats() (written, dropped, failed int64) {
	if s == nil {
		return 0, 0, 0
	}
	return atomic.LoadInt64(&s.written), atomic.LoadInt64(&s.dropped), atomic.LoadInt64(&s.failed)
}

func (s *TrafficCaptureService) consumeLoop() {
	defer s.doneAck.Done()
	for entry := range s.queue { // P-A: queue close 后 range 退出 graceful
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.persist(ctx, entry); err != nil {
			atomic.AddInt64(&s.failed, 1)
			log.Printf("[traffic-capture] persist failed: %v", err)
		} else {
			atomic.AddInt64(&s.written, 1)
		}
		cancel()
	}
}

func (s *TrafficCaptureService) persist(ctx context.Context, e TrafficCaptureEntry) error {
	// nil-safe — test 场景可能 client=nil + Enabled=true (跑 middleware 路径
	// 不真 persist DB). 不会丢用户数据, 因为生产场景 client 永远非 nil.
	if s.client == nil {
		return nil
	}
	inb, inbBytes, inbTrunc := truncateForCapture(e.InboundBody, s.cfg.MaxBytes)
	outb, outbBytes, outbTrunc := truncateForCapture(e.OutboundBody, s.cfg.MaxBytes)
	resp, respBytes, respTrunc := truncateForCapture(e.ResponseBody, s.cfg.MaxBytes)
	// P5 fix: ResponseTotalBytes (middleware writer 累积的真实总大小) override
	// truncateForCapture 拿 e.ResponseBody (已被 cap 截) 的本地判断. 真实 totalBytes
	// 是 middleware writer 的 totalBytes, body 部分已是 cap-clamped.
	if e.ResponseTotalBytes > 0 {
		respBytes = int(e.ResponseTotalBytes)
		if respBytes > s.cfg.MaxBytes {
			respTrunc = true
			if resp != "" && !strings.HasSuffix(resp, "...(truncated)") {
				// 加 truncate 标记跟 truncateForCapture 行为一致
				resp = resp + "...(truncated)"
			}
		}
	}
	// 脱敏 outbound headers (Authorization / x-api-key)
	outHdr := redactSensitiveHeaders(e.OutboundHeaders)
	respHdr := redactSensitiveHeaders(e.ResponseHeaders)
	ts := e.Ts
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	exp := ts.Add(s.cfg.TTL)
	_, err := s.client.TrafficCapture.Create().
		SetTs(ts).
		SetRequestID(e.RequestID).
		SetUpstreamRequestID(e.UpstreamRequestID).
		SetAPIKeyID(e.APIKeyID).
		SetAccountID(e.AccountID).
		SetGroupID(e.GroupID).
		SetPlatform(e.Platform).
		SetAccountType(e.AccountType).
		SetModel(e.Model).
		SetUpstreamStatus(e.UpstreamStatus).
		SetStream(e.Stream).
		SetUseTimeMs(e.UseTimeMS).
		SetInboundBody(inb).
		SetInboundBodyBytes(inbBytes).
		SetInboundBodyTruncated(inbTrunc).
		SetOutboundBody(outb).
		SetOutboundBodyBytes(outbBytes).
		SetOutboundBodyTruncated(outbTrunc).
		SetResponseBody(resp).
		SetResponseBodyBytes(respBytes).
		SetResponseBodyTruncated(respTrunc).
		SetOutboundHeaders(outHdr).
		SetResponseHeaders(respHdr).
		SetErrorKind(e.ErrorKind).
		SetErrorMsg(e.ErrorMsg).
		SetClientIP(e.ClientIP).
		SetUserAgent(e.UserAgent).
		SetExpiresAt(exp).
		Save(ctx)
	return err
}

// truncateForCapture — 截断 body 到 cap, 返 (clamped string, origBytes, truncated).
func truncateForCapture(body []byte, cap int) (string, int, bool) {
	if len(body) == 0 {
		return "", 0, false
	}
	if len(body) <= cap {
		return string(body), len(body), false
	}
	return string(body[:cap]) + "...(truncated)", len(body), true
}

// 这俩 token 在 header 里, 我们留体的时候必须脱敏. value 长一些 (Authorization
// 通常 oat01-/sk-ant-/Bearer xxx), 留前 12 chars + "..." 防完全暴露但还能识别.
var sensitiveHeaderKeys = map[string]bool{
	"authorization":             true,
	"x-api-key":                 true,
	"x-anthropic-api-key":       true,
	"x-openai-api-key":          true,
	"cookie":                    true,
	"set-cookie":                true,
	"anthropic-organization-id": true,
}

func redactSensitiveHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		lk := strings.ToLower(k)
		if sensitiveHeaderKeys[lk] {
			out[k] = redactValue(v)
		} else {
			out[k] = v
		}
	}
	return out
}

func redactValue(v string) string {
	if len(v) <= 12 {
		return "***"
	}
	return v[:12] + "...(redacted, total " + strconv.Itoa(len(v)) + "ch)"
}

// cleanupLoop — 周期删 expires_at < now 的旧 capture (默认 1h tick).
// P-A: 监听 done channel graceful exit.
func (s *TrafficCaptureService) cleanupLoop() {
	defer s.doneAck.Done()
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			n, err := s.client.TrafficCapture.Delete().
				Where(trafficcapture.ExpiresAtLT(time.Now())).
				Exec(ctx)
			cancel()
			if err != nil {
				log.Printf("[traffic-capture] cleanup failed: %v", err)
			} else if n > 0 {
				log.Printf("[traffic-capture] cleanup deleted %d expired", n)
			}
		}
	}
}

// ListRecent — admin 查最近 N 条 (按 ts desc).
func (s *TrafficCaptureService) ListRecent(ctx context.Context, limit int) ([]*dbent.TrafficCapture, error) {
	if s == nil || s.client == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.client.TrafficCapture.Query().
		Order(dbent.Desc(trafficcapture.FieldTs)).
		Limit(limit).
		All(ctx)
}

// GetByRequestID — 按 request_id 查 (一般只 1 条, 多条按 ts desc).
func (s *TrafficCaptureService) GetByRequestID(ctx context.Context, reqID string) ([]*dbent.TrafficCapture, error) {
	if s == nil || s.client == nil || reqID == "" {
		return nil, nil
	}
	return s.client.TrafficCapture.Query().
		Where(trafficcapture.RequestID(reqID)).
		Order(dbent.Desc(trafficcapture.FieldTs)).
		Limit(20).
		All(ctx)
}

// validRequestIDRe 防 admin 查询注入 (我们用 ent 不会 SQL inject 但 reqID 上限长度防 abuse).
var validRequestIDRe = regexp.MustCompile(`^[A-Za-z0-9_\-:.]{1,128}$`)

func IsValidCaptureRequestID(s string) bool {
	return validRequestIDRe.MatchString(s)
}
