package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/robfig/cron/v3"
)

const (
	proxyAutoMaintenanceTickSpec        = "* * * * *"
	proxyAutoMaintenanceRequestTimeout  = 30 * time.Second
	proxyAutoMaintenanceTickTimeout     = 10 * time.Minute
	proxyAutoMaintenanceTestConcurrency = 5
)

type ProxyAutoMaintenanceService struct {
	adminService   AdminService
	settingService *SettingService
	cfg            *config.Config

	cron      *cron.Cron
	startOnce sync.Once
	stopOnce  sync.Once

	stateMu              sync.Mutex
	lastRefillRunAt      time.Time
	lastHealthCheckRunAt time.Time
	proxyFailureCounts   map[int64]int
}

func NewProxyAutoMaintenanceService(adminService AdminService, settingService *SettingService, cfg *config.Config) *ProxyAutoMaintenanceService {
	return &ProxyAutoMaintenanceService{
		adminService:       adminService,
		settingService:     settingService,
		cfg:                cfg,
		proxyFailureCounts: make(map[int64]int),
	}
}

func (s *ProxyAutoMaintenanceService) Start() {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		loc := time.Local
		if s.cfg != nil {
			if parsed, err := time.LoadLocation(s.cfg.Timezone); err == nil && parsed != nil {
				loc = parsed
			}
		}
		c := cron.New(cron.WithLocation(loc))
		_, err := c.AddFunc(proxyAutoMaintenanceTickSpec, func() { s.runTick() })
		if err != nil {
			logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] not started: %v", err)
			return
		}
		s.cron = c
		s.cron.Start()
		logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] started (tick=every minute)")
	})
}

func (s *ProxyAutoMaintenanceService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.cron == nil {
			return
		}
		ctx := s.cron.Stop()
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
			logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] stop timed out")
		}
	})
}

func (s *ProxyAutoMaintenanceService) runTick() {
	// Avoid exact minute boundary burst.
	time.Sleep(13 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), proxyAutoMaintenanceTickTimeout)
	defer cancel()

	settings := s.settingService.GetProxyAutoMaintenanceSettings(ctx)
	if settings == nil || !settings.Enabled {
		return
	}

	now := time.Now()
	if s.shouldRunRefill(now, settings.RefillIntervalMinutes) {
		s.runRefill(ctx, settings)
		s.stateMu.Lock()
		s.lastRefillRunAt = now
		s.stateMu.Unlock()
	}

	if s.shouldRunHealthCheck(now, settings.HealthCheckIntervalMinutes) {
		s.runHealthCheck(ctx, settings)
		s.stateMu.Lock()
		s.lastHealthCheckRunAt = now
		s.stateMu.Unlock()
	}
}

func (s *ProxyAutoMaintenanceService) shouldRunRefill(now time.Time, intervalMinutes int) bool {
	interval := time.Duration(clampProxyAutoRefillIntervalMinutes(intervalMinutes)) * time.Minute
	s.stateMu.Lock()
	last := s.lastRefillRunAt
	s.stateMu.Unlock()
	if last.IsZero() {
		return true
	}
	return now.Sub(last) >= interval
}

func (s *ProxyAutoMaintenanceService) shouldRunHealthCheck(now time.Time, intervalMinutes int) bool {
	interval := time.Duration(clampProxyAutoHealthCheckIntervalMinutes(intervalMinutes)) * time.Minute
	s.stateMu.Lock()
	last := s.lastHealthCheckRunAt
	s.stateMu.Unlock()
	if last.IsZero() {
		return true
	}
	return now.Sub(last) >= interval
}

func (s *ProxyAutoMaintenanceService) runRefill(ctx context.Context, settings *ProxyAutoMaintenanceSettings) {
	activeProxies, err := s.adminService.GetAllProxies(ctx)
	if err != nil {
		logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] list active proxies failed: %v", err)
		return
	}

	threshold := clampProxyAutoPoolLowWatermark(settings.PoolLowWatermark)
	if len(activeProxies) >= threshold {
		return
	}

	if !settings.SourceEnabled {
		logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] source disabled, skip refill")
		return
	}
	if strings.TrimSpace(settings.ExtractURL) == "" {
		logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] extract URL empty, skip refill")
		return
	}

	inputs, err := s.fetchAndParseProxyInputs(ctx, settings)
	if err != nil {
		s.recordSourceFailure(ctx, settings, err)
		return
	}
	if len(inputs) == 0 {
		s.recordSourceFailure(ctx, settings, errors.New("extract response contains no valid proxy entries"))
		return
	}

	created := 0
	skipped := 0
	for idx := range inputs {
		input := inputs[idx]
		exists, existsErr := s.adminService.CheckProxyExists(ctx, input.Host, input.Port, input.Username, input.Password)
		if existsErr != nil {
			skipped++
			continue
		}
		if exists {
			skipped++
			continue
		}
		if _, createErr := s.adminService.CreateProxy(ctx, &input); createErr != nil {
			skipped++
			continue
		}
		created++
	}

	if created == 0 {
		s.recordSourceFailure(ctx, settings, fmt.Errorf("import failed: all %d proxies skipped", len(inputs)))
		return
	}

	s.recordSourceSuccess(ctx)
	logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] refill done created=%d skipped=%d threshold=%d active_before=%d", created, skipped, threshold, len(activeProxies))
}

func (s *ProxyAutoMaintenanceService) runHealthCheck(ctx context.Context, settings *ProxyAutoMaintenanceSettings) {
	activeProxies, err := s.adminService.GetAllProxies(ctx)
	if err != nil {
		logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] list active proxies for health check failed: %v", err)
		return
	}
	if len(activeProxies) == 0 {
		return
	}

	threshold := clampProxyAutoDeadFailureThreshold(settings.DeadFailureThreshold)
	ids := make([]int64, 0, len(activeProxies))
	for idx := range activeProxies {
		ids = append(ids, activeProxies[idx].ID)
	}

	jobs := make(chan int64)
	var wg sync.WaitGroup
	workerCount := proxyAutoMaintenanceTestConcurrency
	if len(ids) < workerCount {
		workerCount = len(ids)
	}

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				result, testErr := s.adminService.TestProxy(ctx, id)
				if testErr != nil || result == nil || !result.Success {
					s.onProxyProbeFailed(ctx, id, threshold)
					continue
				}
				s.onProxyProbeSuccess(id)
			}
		}()
	}

	for _, id := range ids {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
}

func (s *ProxyAutoMaintenanceService) onProxyProbeSuccess(id int64) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	delete(s.proxyFailureCounts, id)
}

func (s *ProxyAutoMaintenanceService) onProxyProbeFailed(ctx context.Context, id int64, threshold int) {
	s.stateMu.Lock()
	count := s.proxyFailureCounts[id] + 1
	s.proxyFailureCounts[id] = count
	s.stateMu.Unlock()

	if count < threshold {
		return
	}

	err := s.adminService.DeleteProxy(ctx, id)
	if err != nil {
		if errors.Is(err, ErrProxyInUse) {
			logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] proxy=%d dead but in use, skip delete", id)
		} else {
			logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] delete dead proxy=%d failed: %v", id, err)
		}
		return
	}

	s.stateMu.Lock()
	delete(s.proxyFailureCounts, id)
	s.stateMu.Unlock()
	logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] deleted dead proxy=%d after %d failures", id, threshold)
}

func (s *ProxyAutoMaintenanceService) fetchAndParseProxyInputs(ctx context.Context, settings *ProxyAutoMaintenanceSettings) ([]CreateProxyInput, error) {
	client, err := httpclient.GetClient(httpclient.Options{
		ProxyURL:              settings.ExtractProxyURL,
		Timeout:               proxyAutoMaintenanceRequestTimeout,
		ResponseHeaderTimeout: proxyAutoMaintenanceRequestTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("build extract client: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, settings.ExtractURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build extract request: %w", err)
	}
	req.Header.Set("Accept", "application/json,text/plain,*/*")
	req.Header.Set("User-Agent", "sub2api-proxy-auto-maintenance/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request extract API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("extract API returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read extract API response: %w", err)
	}

	return parseProxyInputs(body), nil
}

func (s *ProxyAutoMaintenanceService) recordSourceSuccess(ctx context.Context) {
	err := s.settingService.SetProxyAutoSourceRuntime(ctx, true, 0, "", time.Now().Unix())
	if err != nil {
		logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] update source runtime(success) failed: %v", err)
	}
}

func (s *ProxyAutoMaintenanceService) recordSourceFailure(ctx context.Context, settings *ProxyAutoMaintenanceSettings, err error) {
	threshold := clampProxyAutoSourceFailureThreshold(settings.SourceFailureThreshold)
	nextFailures := clampProxyAutoSourceConsecutiveFailures(settings.SourceConsecutiveFailures + 1)
	sourceEnabled := settings.SourceEnabled
	if nextFailures >= threshold {
		sourceEnabled = false
	}
	updateErr := s.settingService.SetProxyAutoSourceRuntime(ctx, sourceEnabled, nextFailures, err.Error(), settings.SourceLastSuccessAtUnix)
	if updateErr != nil {
		logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] update source runtime(failure) failed: %v", updateErr)
	}
	if !sourceEnabled {
		logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] source disabled after failures=%d threshold=%d err=%v", nextFailures, threshold, err)
		return
	}
	logger.LegacyPrintf("service.proxy_auto_maintenance", "[ProxyAutoMaintenance] source failure failures=%d threshold=%d err=%v", nextFailures, threshold, err)
}

func parseProxyInputs(body []byte) []CreateProxyInput {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return nil
	}

	if fromJSON := parseProxyInputsFromJSON([]byte(text)); len(fromJSON) > 0 {
		return fromJSON
	}

	lines := strings.Split(text, "\n")
	parsed := make([]CreateProxyInput, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		input, ok := parseProxyInputLine(line)
		if !ok {
			continue
		}
		key := buildProxyIdentityKey(input)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		parsed = append(parsed, input)
	}
	return parsed
}

func parseProxyInputsFromJSON(body []byte) []CreateProxyInput {
	var arr []string
	if err := json.Unmarshal(body, &arr); err == nil {
		lines := make([]string, 0, len(arr))
		lines = append(lines, arr...)
		return parseProxyInputs([]byte(strings.Join(lines, "\n")))
	}

	var wrapped struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && len(wrapped.Data) > 0 {
		return parseProxyInputs([]byte(strings.Join(wrapped.Data, "\n")))
	}
	return nil
}

func parseProxyInputLine(raw string) (CreateProxyInput, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return CreateProxyInput{}, false
	}
	u, err := url.Parse(trimmed)
	if err != nil || u == nil {
		return CreateProxyInput{}, false
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	switch scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return CreateProxyInput{}, false
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return CreateProxyInput{}, false
	}
	portRaw := strings.TrimSpace(u.Port())
	if portRaw == "" {
		return CreateProxyInput{}, false
	}
	port, err := strconv.Atoi(portRaw)
	if err != nil || port < 1 || port > 65535 {
		return CreateProxyInput{}, false
	}
	username := ""
	password := ""
	if u.User != nil {
		username = strings.TrimSpace(u.User.Username())
		if v, ok := u.User.Password(); ok {
			password = strings.TrimSpace(v)
		}
	}
	return CreateProxyInput{
		Name:     "default",
		Protocol: scheme,
		Host:     host,
		Port:     port,
		Username: username,
		Password: password,
	}, true
}

func buildProxyIdentityKey(input CreateProxyInput) string {
	return strings.Join([]string{
		strings.ToLower(strings.TrimSpace(input.Protocol)),
		strings.ToLower(strings.TrimSpace(input.Host)),
		strconv.Itoa(input.Port),
		strings.TrimSpace(input.Username),
		strings.TrimSpace(input.Password),
	}, "|")
}
