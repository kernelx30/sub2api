//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyServiceCreateDoesNotPersistUnavailableOptionalInstructionsOptIn(t *testing.T) {
	groupID := int64(17)
	repo := &apiKeyRepoStub{allowCreate: true}
	svc := NewAPIKeyService(
		repo,
		&userRepoStub{user: &User{ID: 5, Status: StatusActive}},
		&groupRepoStubForAdmin{getByID: &Group{
			ID:                          groupID,
			Platform:                    PlatformOpenAI,
			OptionalInstructionsEnabled: false,
			OptionalInstructions:        "admin",
		}},
		nil,
		nil,
		nil,
		&config.Config{},
	)

	created, err := svc.Create(context.Background(), 5, CreateAPIKeyRequest{
		Name:                        "key",
		GroupID:                     &groupID,
		OptionalInstructionsEnabled: true,
	})
	require.NoError(t, err)
	require.False(t, created.OptionalInstructionsEnabled)
	require.Len(t, repo.createdKeys, 1)
	require.False(t, repo.createdKeys[0].OptionalInstructionsEnabled)
}

func TestAPIKeyServiceCreateAllowsOptInForSubscribedOpenAIGroup(t *testing.T) {
	groupID := int64(23)
	userID := int64(5)
	repo := &apiKeyRepoStub{allowCreate: true}
	subRepo := &userSubRepoStubForGroupUpdate{getActiveSub: &UserSubscription{
		UserID:  userID,
		GroupID: groupID,
	}}
	svc := NewAPIKeyService(
		repo,
		&userRepoStub{user: &User{ID: userID, Status: StatusActive}},
		&groupRepoStubForAdmin{getByID: &Group{
			ID:                          groupID,
			Platform:                    PlatformOpenAI,
			SubscriptionType:            SubscriptionTypeSubscription,
			OptionalInstructionsEnabled: true,
			OptionalInstructions:        "admin",
		}},
		subRepo,
		nil,
		nil,
		&config.Config{},
	)

	created, err := svc.Create(context.Background(), userID, CreateAPIKeyRequest{
		Name:                        "subscription-key",
		GroupID:                     &groupID,
		OptionalInstructionsEnabled: true,
	})
	require.NoError(t, err)
	require.True(t, subRepo.called)
	require.Equal(t, userID, subRepo.calledUserID)
	require.Equal(t, groupID, subRepo.calledGroupID)
	require.True(t, created.OptionalInstructionsEnabled)
	require.Len(t, repo.createdKeys, 1)
	require.True(t, repo.createdKeys[0].OptionalInstructionsEnabled)
}

func TestAPIKeyServiceUpdatePersistsOptInOnlyForOfferingGroup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		group *Group
		want  bool
	}{
		{
			name: "offering group",
			group: &Group{
				Platform:                    PlatformOpenAI,
				OptionalInstructionsEnabled: true,
				OptionalInstructions:        "admin",
			},
			want: true,
		},
		{
			name: "disabled group",
			group: &Group{
				Platform:                    PlatformOpenAI,
				OptionalInstructionsEnabled: false,
				OptionalInstructions:        "admin",
			},
			want: false,
		},
		{
			name: "blank instructions",
			group: &Group{
				Platform:                    PlatformOpenAI,
				OptionalInstructionsEnabled: true,
				OptionalInstructions:        "  ",
			},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &apiKeyRepoStub{apiKey: &APIKey{
				ID:     9,
				UserID: 5,
				Key:    "sk-test",
				Status: StatusActive,
				Group:  tc.group,
			}}
			svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, nil)
			enabled := true

			updated, err := svc.Update(context.Background(), 9, 5, UpdateAPIKeyRequest{
				OptionalInstructionsEnabled: &enabled,
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, updated.OptionalInstructionsEnabled)
			require.Len(t, repo.updatedKeys, 1)
			require.Equal(t, tc.want, repo.updatedKeys[0].OptionalInstructionsEnabled)
		})
	}
}

func TestAPIKeyServiceUpdateExplicitlyUnbindsGroupAndClearsOptIn(t *testing.T) {
	groupID := int64(17)
	oldGroup := &Group{
		ID:                          groupID,
		Platform:                    PlatformOpenAI,
		OptionalInstructionsEnabled: true,
		OptionalInstructions:        "admin",
	}
	repo := &apiKeyRepoStub{apiKey: &APIKey{
		ID:                          9,
		UserID:                      5,
		Key:                         "sk-test",
		Status:                      StatusActive,
		GroupID:                     &groupID,
		Group:                       oldGroup,
		OptionalInstructionsEnabled: true,
	}}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, nil)

	updated, err := svc.Update(context.Background(), 9, 5, UpdateAPIKeyRequest{GroupIDSet: true})
	require.NoError(t, err)
	require.Nil(t, updated.GroupID)
	require.Nil(t, updated.Group)
	require.False(t, updated.OptionalInstructionsEnabled)
	require.Len(t, repo.updatedKeys, 1)
	require.Nil(t, repo.updatedKeys[0].GroupID)
	require.Nil(t, repo.updatedKeys[0].Group)
	require.False(t, repo.updatedKeys[0].OptionalInstructionsEnabled)
}

func TestAPIKeyServiceUpdateRefreshesReturnedGroupRelation(t *testing.T) {
	oldGroupID := int64(17)
	newGroupID := int64(18)
	oldGroup := &Group{ID: oldGroupID, Name: "old", Platform: PlatformOpenAI}
	newGroup := &Group{
		ID:                          newGroupID,
		Name:                        "new",
		Platform:                    PlatformOpenAI,
		OptionalInstructionsEnabled: true,
		OptionalInstructions:        "admin",
	}
	repo := &apiKeyRepoStub{apiKey: &APIKey{
		ID:      9,
		UserID:  5,
		Key:     "sk-test",
		Status:  StatusActive,
		GroupID: &oldGroupID,
		Group:   oldGroup,
	}}
	svc := NewAPIKeyService(
		repo,
		&userRepoStub{user: &User{ID: 5, Status: StatusActive}},
		&groupRepoStubForAdmin{getByID: newGroup},
		nil,
		nil,
		nil,
		nil,
	)

	updated, err := svc.Update(context.Background(), 9, 5, UpdateAPIKeyRequest{GroupID: &newGroupID})
	require.NoError(t, err)
	require.NotNil(t, updated.GroupID)
	require.Equal(t, newGroupID, *updated.GroupID)
	require.Same(t, newGroup, updated.Group)
	require.Equal(t, "new", updated.Group.Name)
}
