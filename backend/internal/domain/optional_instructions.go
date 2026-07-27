package domain

import (
	"errors"
	"unicode/utf8"
)

const OptionalInstructionsMaxCharacters = 16 * 1024

func ValidateOptionalInstructions(instructions string) error {
	if utf8.RuneCountInString(instructions) > OptionalInstructionsMaxCharacters {
		return errors.New("optional instructions must not exceed 16384 characters")
	}
	return nil
}
