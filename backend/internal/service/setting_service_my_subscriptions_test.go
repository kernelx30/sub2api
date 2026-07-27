//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestMySubscriptionsEnabledDefaultsOnAndPersistsOff(t *testing.T) {
	svc := NewSettingService(&settingUpdateRepoStub{}, &config.Config{})
	require.True(t, svc.parseSettings(map[string]string{}).MySubscriptionsEnabled)
	require.False(t, svc.parseSettings(map[string]string{
		SettingKeyMySubscriptionsEnabled: "false",
	}).MySubscriptionsEnabled)

	repo := &settingUpdateRepoStub{}
	svc = NewSettingService(repo, &config.Config{})
	require.NoError(t, svc.UpdateSettings(context.Background(), &SystemSettings{
		MySubscriptionsEnabled: false,
	}))
	require.Equal(t, "false", repo.updates[SettingKeyMySubscriptionsEnabled])
}
