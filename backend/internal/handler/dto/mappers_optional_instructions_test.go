package dto

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGroupFromServiceOffersOptionalInstructionsOnlyForOpenAI(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform string
		want     bool
	}{
		{name: "openai", platform: service.PlatformOpenAI, want: true},
		{name: "anthropic", platform: service.PlatformAnthropic, want: false},
		{name: "gemini", platform: service.PlatformGemini, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := GroupFromService(&service.Group{
				Platform:                    tc.platform,
				OptionalInstructionsEnabled: true,
				OptionalInstructions:        "admin prompt",
			})
			require.Equal(t, tc.want, got.OptionalInstructionsAvailable)
		})
	}
}
