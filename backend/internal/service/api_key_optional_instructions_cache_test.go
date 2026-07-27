package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthSnapshotPreservesOptionalInstructions(t *testing.T) {
	svc := &APIKeyService{}
	key := &APIKey{
		ID:                          7,
		Key:                         "sk-test",
		OptionalInstructionsEnabled: true,
		User:                        &User{ID: 11},
		Group: &Group{
			ID:                          13,
			OptionalInstructionsEnabled: true,
			OptionalInstructions:        "admin prompt",
		},
	}

	snapshot := svc.snapshotFromAPIKey(context.Background(), key)
	require.Equal(t, apiKeyAuthSnapshotVersion, snapshot.Version)
	require.True(t, snapshot.OptionalInstructionsEnabled)
	require.NotNil(t, snapshot.Group)
	require.True(t, snapshot.Group.OptionalInstructionsEnabled)
	require.Equal(t, "admin prompt", snapshot.Group.OptionalInstructions)

	restored := svc.snapshotToAPIKey(key.Key, snapshot)
	require.True(t, restored.OptionalInstructionsEnabled)
	require.NotNil(t, restored.Group)
	require.True(t, restored.Group.OptionalInstructionsEnabled)
	require.Equal(t, "admin prompt", restored.Group.OptionalInstructions)
}
