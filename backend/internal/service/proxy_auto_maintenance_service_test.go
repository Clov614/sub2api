package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type proxyAutoRuntimeRepoStub struct {
	setMultipleFn func(ctx context.Context, settings map[string]string) error
}

func (s *proxyAutoRuntimeRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	panic("unexpected Get call")
}

func (s *proxyAutoRuntimeRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	panic("unexpected GetValue call")
}

func (s *proxyAutoRuntimeRepoStub) Set(ctx context.Context, key, value string) error {
	panic("unexpected Set call")
}

func (s *proxyAutoRuntimeRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	panic("unexpected GetMultiple call")
}

func (s *proxyAutoRuntimeRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	if s.setMultipleFn == nil {
		panic("unexpected SetMultiple call")
	}
	return s.setMultipleFn(ctx, settings)
}

func (s *proxyAutoRuntimeRepoStub) GetAll(ctx context.Context) (map[string]string, error) {
	panic("unexpected GetAll call")
}

func (s *proxyAutoRuntimeRepoStub) Delete(ctx context.Context, key string) error {
	panic("unexpected Delete call")
}

type proxyAutoAdminServiceStub struct {
	AdminService
	deleteCalls []int64
	deleteErr   error
}

func (s *proxyAutoAdminServiceStub) DeleteProxy(ctx context.Context, id int64) error {
	s.deleteCalls = append(s.deleteCalls, id)
	return s.deleteErr
}

func TestParseProxyInputs_TextDeduplicatesAndSkipsInvalid(t *testing.T) {
	body := []byte(`
http://user:pass@1.1.1.1:8080
http://user:pass@1.1.1.1:8080
https://2.2.2.2:443
socks5://3.3.3.3:1080
not-a-url
ftp://4.4.4.4:21
http://5.5.5.5
`)

	got := parseProxyInputs(body)
	require.Len(t, got, 3)
	require.Equal(t, "http", got[0].Protocol)
	require.Equal(t, "1.1.1.1", got[0].Host)
	require.Equal(t, 8080, got[0].Port)
	require.Equal(t, "user", got[0].Username)
	require.Equal(t, "pass", got[0].Password)
	require.Equal(t, "https", got[1].Protocol)
	require.Equal(t, "socks5", got[2].Protocol)
}

func TestParseProxyInputs_JSONFormats(t *testing.T) {
	arr := parseProxyInputs([]byte(`["http://1.1.1.1:80","https://2.2.2.2:443"]`))
	require.Len(t, arr, 2)
	require.Equal(t, "1.1.1.1", arr[0].Host)
	require.Equal(t, "2.2.2.2", arr[1].Host)

	wrapped := parseProxyInputs([]byte(`{"data":["http://3.3.3.3:80","socks5://4.4.4.4:1080"]}`))
	require.Len(t, wrapped, 2)
	require.Equal(t, "3.3.3.3", wrapped[0].Host)
	require.Equal(t, "socks5", wrapped[1].Protocol)
}

func TestProxyAutoMaintenance_RecordSourceFailure_DisablesAtThreshold(t *testing.T) {
	captured := map[string]string{}
	repo := &proxyAutoRuntimeRepoStub{
		setMultipleFn: func(ctx context.Context, settings map[string]string) error {
			for k, v := range settings {
				captured[k] = v
			}
			return nil
		},
	}
	settingSvc := NewSettingService(repo, nil)
	svc := &ProxyAutoMaintenanceService{settingService: settingSvc}

	svc.recordSourceFailure(context.Background(), &ProxyAutoMaintenanceSettings{
		SourceEnabled:             true,
		SourceFailureThreshold:    3,
		SourceConsecutiveFailures: 2,
		SourceLastSuccessAtUnix:   111,
	}, context.DeadlineExceeded)

	require.Equal(t, "false", captured[SettingKeyProxyAutoSourceEnabled])
	require.Equal(t, "3", captured[SettingKeyProxyAutoSourceConsecutiveFailures])
	require.Equal(t, context.DeadlineExceeded.Error(), captured[SettingKeyProxyAutoSourceLastError])
	require.Equal(t, "111", captured[SettingKeyProxyAutoSourceLastSuccessAtUnix])
}

func TestProxyAutoMaintenance_RecordSourceFailure_KeepsEnabledBelowThreshold(t *testing.T) {
	captured := map[string]string{}
	repo := &proxyAutoRuntimeRepoStub{
		setMultipleFn: func(ctx context.Context, settings map[string]string) error {
			for k, v := range settings {
				captured[k] = v
			}
			return nil
		},
	}
	settingSvc := NewSettingService(repo, nil)
	svc := &ProxyAutoMaintenanceService{settingService: settingSvc}

	svc.recordSourceFailure(context.Background(), &ProxyAutoMaintenanceSettings{
		SourceEnabled:             true,
		SourceFailureThreshold:    3,
		SourceConsecutiveFailures: 1,
	}, context.Canceled)

	require.Equal(t, "true", captured[SettingKeyProxyAutoSourceEnabled])
	require.Equal(t, "2", captured[SettingKeyProxyAutoSourceConsecutiveFailures])
	require.Equal(t, context.Canceled.Error(), captured[SettingKeyProxyAutoSourceLastError])
}

func TestProxyAutoMaintenance_OnProxyProbeFailed_SkipsDeleteWhenBelowThreshold(t *testing.T) {
	adminStub := &proxyAutoAdminServiceStub{}
	svc := &ProxyAutoMaintenanceService{
		adminService:       adminStub,
		proxyFailureCounts: map[int64]int{},
	}

	svc.onProxyProbeFailed(context.Background(), 7, 3)

	require.Equal(t, []int64(nil), adminStub.deleteCalls)
	require.Equal(t, 1, svc.proxyFailureCounts[7])
}

func TestProxyAutoMaintenance_OnProxyProbeFailed_InUseKeepsFailureCount(t *testing.T) {
	adminStub := &proxyAutoAdminServiceStub{deleteErr: ErrProxyInUse}
	svc := &ProxyAutoMaintenanceService{
		adminService:       adminStub,
		proxyFailureCounts: map[int64]int{8: 2},
	}

	svc.onProxyProbeFailed(context.Background(), 8, 3)

	require.Equal(t, []int64{8}, adminStub.deleteCalls)
	require.Equal(t, 3, svc.proxyFailureCounts[8])
}

func TestProxyAutoMaintenance_OnProxyProbeFailed_DeletesAndClearsCount(t *testing.T) {
	adminStub := &proxyAutoAdminServiceStub{}
	svc := &ProxyAutoMaintenanceService{
		adminService:       adminStub,
		proxyFailureCounts: map[int64]int{9: 2},
	}

	svc.onProxyProbeFailed(context.Background(), 9, 3)

	require.Equal(t, []int64{9}, adminStub.deleteCalls)
	_, exists := svc.proxyFailureCounts[9]
	require.False(t, exists)
}
