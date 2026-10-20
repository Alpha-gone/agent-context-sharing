package model

import (
	"errors"
	"strings"
	"testing"
)

func TestRegistrationKey(t *testing.T) {
	for _, sample := range []struct {
		name, key string
		valid     bool
	}{
		{"minimum", strings.Repeat("a", 16), true}, {"maximum", strings.Repeat("Z", 128), true}, {"alphabet", "Az09_-Az09_-Az09", true},
		{"empty", "", false}, {"short", strings.Repeat("a", 15), false}, {"long", strings.Repeat("a", 129), false},
		{"space", "registration key", false}, {"newline", "registration_key\n", false}, {"nul", "registration_key\x00", false},
		{"unicode", "등록키등록키등록키등록키등록키등록키", false}, {"invalid utf8", strings.Repeat("a", 16) + "\xff", false}, {"slash", "registration/key", false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			err := ValidateRegistrationKey(sample.key)
			if sample.valid {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if field, ok := errors.AsType[FieldError](err); !ok || field.Field != "registration_key" || strings.Contains(field.Message, sample.key) && sample.key != "" {
				t.Fatal("등록 키 오류 필드·원문 비노출 실패")
			}
		})
	}
}
