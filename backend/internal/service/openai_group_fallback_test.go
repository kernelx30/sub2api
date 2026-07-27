package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type openAIAvailabilityFallbackGroupRepoStub struct {
	GroupRepository
	groups map[int64]*Group
}

func (s *openAIAvailabilityFallbackGroupRepoStub) GetByID(_ context.Context, id int64) (*Group, error) {
	group, ok := s.groups[id]
	if !ok {
		return nil, ErrGroupNotFound
	}
	return group, nil
}

func (s *openAIAvailabilityFallbackGroupRepoStub) GetByIDLite(ctx context.Context, id int64) (*Group, error) {
	return s.GetByID(ctx, id)
}

type openAIAvailabilityFallbackSubscriptionRepoStub struct {
	UserSubscriptionRepository
	subscription *UserSubscription
	err          error
}

func (s *openAIAvailabilityFallbackSubscriptionRepoStub) GetActiveByUserIDAndGroupID(_ context.Context, _, _ int64) (*UserSubscription, error) {
	return s.subscription, s.err
}

func TestResolveOpenAIAvailabilityFallback_ClonesActualGroupContext(t *testing.T) {
	primaryID := int64(5)
	fallbackID := int64(9)
	override := 5
	primary := &Group{ID: primaryID, Platform: PlatformOpenAI, Status: StatusActive, FallbackGroupID: &fallbackID}
	fallback := &Group{ID: fallbackID, Platform: PlatformOpenAI, Status: StatusActive, RateMultiplier: 0.09}
	repo := &openAIAvailabilityFallbackGroupRepoStub{groups: map[int64]*Group{fallbackID: fallback}}
	svc := &OpenAIGatewayService{schedulerSnapshot: &SchedulerSnapshotService{groupRepo: repo}}
	apiKey := &APIKey{
		ID:      100,
		GroupID: &primaryID,
		Group:   primary,
		User:    &User{ID: 200, UserGroupRPMOverride: &override},
	}

	resolved, err := svc.ResolveOpenAIAvailabilityFallback(context.Background(), apiKey)

	require.NoError(t, err)
	require.NotNil(t, resolved)
	require.Equal(t, fallbackID, *resolved.APIKey.GroupID)
	require.Same(t, fallback, resolved.APIKey.Group)
	require.Nil(t, resolved.APIKey.User.UserGroupRPMOverride)
	require.Equal(t, &override, apiKey.User.UserGroupRPMOverride, "auth snapshot must remain unchanged")
	require.Nil(t, resolved.Subscription)
}

func TestResolveOpenAIAvailabilityFallback_LoadsTargetSubscription(t *testing.T) {
	primaryID := int64(5)
	fallbackID := int64(9)
	primary := &Group{ID: primaryID, Platform: PlatformOpenAI, Status: StatusActive, FallbackGroupID: &fallbackID}
	fallback := &Group{ID: fallbackID, Platform: PlatformOpenAI, Status: StatusActive, SubscriptionType: SubscriptionTypeSubscription}
	subscription := &UserSubscription{ID: 300, UserID: 200, GroupID: fallbackID}
	repo := &openAIAvailabilityFallbackGroupRepoStub{groups: map[int64]*Group{fallbackID: fallback}}
	svc := &OpenAIGatewayService{
		schedulerSnapshot: &SchedulerSnapshotService{groupRepo: repo},
		userSubRepo:       &openAIAvailabilityFallbackSubscriptionRepoStub{subscription: subscription},
	}

	resolved, err := svc.ResolveOpenAIAvailabilityFallback(context.Background(), &APIKey{
		GroupID: &primaryID,
		Group:   primary,
		User:    &User{ID: 200},
	})

	require.NoError(t, err)
	require.Same(t, subscription, resolved.Subscription)
}

func TestResolveOpenAIAvailabilityFallback_RejectsInvalidTarget(t *testing.T) {
	primaryID := int64(5)
	fallbackID := int64(9)
	primary := &Group{ID: primaryID, Platform: PlatformOpenAI, Status: StatusActive, FallbackGroupID: &fallbackID}

	tests := []struct {
		name     string
		fallback *Group
	}{
		{name: "wrong platform", fallback: &Group{ID: fallbackID, Platform: PlatformAnthropic, Status: StatusActive}},
		{name: "inactive", fallback: &Group{ID: fallbackID, Platform: PlatformOpenAI, Status: StatusDisabled}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &openAIAvailabilityFallbackGroupRepoStub{groups: map[int64]*Group{fallbackID: tt.fallback}}
			svc := &OpenAIGatewayService{schedulerSnapshot: &SchedulerSnapshotService{groupRepo: repo}}

			resolved, err := svc.ResolveOpenAIAvailabilityFallback(context.Background(), &APIKey{
				GroupID: &primaryID,
				Group:   primary,
				User:    &User{ID: 200},
			})

			require.Nil(t, resolved)
			require.ErrorIs(t, err, ErrOpenAIAvailabilityFallbackInvalid)
		})
	}
}
