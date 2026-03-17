//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type proxyAutoSettingRepoStub struct {
	getAllFn      func(ctx context.Context) (map[string]string, error)
	setMultipleFn func(ctx context.Context, settings map[string]string) error
}

func (s *proxyAutoSettingRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	panic("unexpected Get call")
}

func (s *proxyAutoSettingRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	panic("unexpected GetValue call")
}

func (s *proxyAutoSettingRepoStub) Set(ctx context.Context, key, value string) error {
	panic("unexpected Set call")
}

func (s *proxyAutoSettingRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	panic("unexpected GetMultiple call")
}

func (s *proxyAutoSettingRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	if s.setMultipleFn == nil {
		panic("unexpected SetMultiple call")
	}
	return s.setMultipleFn(ctx, settings)
}

func (s *proxyAutoSettingRepoStub) GetAll(ctx context.Context) (map[string]string, error) {
	if s.getAllFn == nil {
		panic("unexpected GetAll call")
	}
	return s.getAllFn(ctx)
}

func (s *proxyAutoSettingRepoStub) Delete(ctx context.Context, key string) error {
	panic("unexpected Delete call")
}

func TestSettingService_GetProxyAutoMaintenanceSettings_UsesDefaultsOnGetAllError(t *testing.T) {
	repo := &proxyAutoSettingRepoStub{
		getAllFn: func(ctx context.Context) (map[string]string, error) {
			return nil, errors.New("db down")
		},
	}
	svc := NewSettingService(repo, &config.Config{})

	got := svc.GetProxyAutoMaintenanceSettings(context.Background())
	require.Equal(t, DefaultProxyAutoMaintenanceSettings(), got)
}

func TestSettingService_SetProxyAutoSourceRuntime_TrimsAndPersistsConsecutiveFailures(t *testing.T) {
	captured := map[string]string{}
	repo := &proxyAutoSettingRepoStub{
		setMultipleFn: func(ctx context.Context, settings map[string]string) error {
			for k, v := range settings {
				captured[k] = v
			}
			return nil
		},
	}
	svc := NewSettingService(repo, &config.Config{})

	err := svc.SetProxyAutoSourceRuntime(context.Background(), false, 9999, "  fail reason  ", 123)
	require.NoError(t, err)
	require.Equal(t, "false", captured[SettingKeyProxyAutoSourceEnabled])
	require.Equal(t, "9999", captured[SettingKeyProxyAutoSourceConsecutiveFailures])
	require.Equal(t, "fail reason", captured[SettingKeyProxyAutoSourceLastError])
	require.Equal(t, "123", captured[SettingKeyProxyAutoSourceLastSuccessAtUnix])
}
