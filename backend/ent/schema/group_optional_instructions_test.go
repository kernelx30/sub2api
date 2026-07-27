package schema

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestGroupOptionalInstructionsValidatorCountsUnicodeCharacters(t *testing.T) {
	var validator func(string) error
	for _, schemaField := range (Group{}).Fields() {
		descriptor := schemaField.Descriptor()
		if descriptor.Name == "optional_instructions" {
			require.Len(t, descriptor.Validators, 1)
			var ok bool
			validator, ok = descriptor.Validators[0].(func(string) error)
			require.True(t, ok)
			break
		}
	}
	require.NotNil(t, validator)
	require.NoError(t, validator(strings.Repeat("界", domain.OptionalInstructionsMaxCharacters)))
	require.Error(t, validator(strings.Repeat("界", domain.OptionalInstructionsMaxCharacters+1)))
}
