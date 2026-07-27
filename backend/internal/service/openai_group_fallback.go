package service

import (
	"context"
	"errors"
	"fmt"
)

var ErrOpenAIAvailabilityFallbackInvalid = errors.New("invalid OpenAI availability fallback group")

type OpenAIAvailabilityFallback struct {
	APIKey       *APIKey
	Subscription *UserSubscription
}

// ResolveOpenAIAvailabilityFallback resolves the single target used when an
// OpenAI group's schedulable accounts are unavailable. Chaining is handled by
// configuration validation; a request activates at most one fallback group.
func (s *OpenAIGatewayService) ResolveOpenAIAvailabilityFallback(ctx context.Context, apiKey *APIKey) (*OpenAIAvailabilityFallback, error) {
	if s == nil || apiKey == nil || apiKey.Group == nil || apiKey.Group.Platform != PlatformOpenAI ||
		apiKey.Group.FallbackGroupID == nil || *apiKey.Group.FallbackGroupID <= 0 {
		return nil, nil
	}

	fallbackID := *apiKey.Group.FallbackGroupID
	if apiKey.GroupID != nil && *apiKey.GroupID == fallbackID {
		return nil, fmt.Errorf("%w: group %d points to itself", ErrOpenAIAvailabilityFallbackInvalid, fallbackID)
	}

	group, err := s.resolveOpenAIGroupByID(ctx, fallbackID)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve group %d: %v", ErrOpenAIAvailabilityFallbackInvalid, fallbackID, err)
	}
	if group == nil {
		return nil, fmt.Errorf("%w: group %d not found", ErrOpenAIAvailabilityFallbackInvalid, fallbackID)
	}
	if group.Platform != PlatformOpenAI {
		return nil, fmt.Errorf("%w: group %d platform is %s", ErrOpenAIAvailabilityFallbackInvalid, fallbackID, group.Platform)
	}
	if !group.IsActive() {
		return nil, fmt.Errorf("%w: group %d is inactive", ErrOpenAIAvailabilityFallbackInvalid, fallbackID)
	}

	resolved := &OpenAIAvailabilityFallback{APIKey: CloneAPIKeyWithGroup(apiKey, group)}
	if !group.IsSubscriptionType() {
		return resolved, nil
	}
	if s.userSubRepo == nil || resolved.APIKey.User == nil {
		return nil, fmt.Errorf("resolve fallback subscription: %w", ErrSubscriptionNotFound)
	}

	subscription, err := s.userSubRepo.GetActiveByUserIDAndGroupID(ctx, resolved.APIKey.User.ID, group.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve fallback subscription: %w", err)
	}
	resolved.Subscription = subscription
	return resolved, nil
}

func (s *OpenAIGatewayService) resolveOpenAIGroupByID(ctx context.Context, groupID int64) (*Group, error) {
	if s.schedulerSnapshot != nil {
		group, err := s.schedulerSnapshot.GetGroupByID(ctx, groupID)
		if err != nil || group != nil {
			return group, err
		}
	}
	if s.channelService != nil && s.channelService.groupRepo != nil {
		return s.channelService.groupRepo.GetByIDLite(ctx, groupID)
	}
	return nil, ErrGroupNotFound
}
