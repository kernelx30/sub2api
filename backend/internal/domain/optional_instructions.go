package domain

import (
	"fmt"
	"unicode/utf8"
)

const OptionalInstructionsMaxCharacters = 64 * 1024

func ValidateOptionalInstructions(instructions string) error {
	if utf8.RuneCountInString(instructions) > OptionalInstructionsMaxCharacters {
		return fmt.Errorf("optional instructions must not exceed %d characters", OptionalInstructionsMaxCharacters)
	}
	return nil
}
