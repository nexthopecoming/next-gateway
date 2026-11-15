package model_test

import (
	"strings"
	"testing"

	"github.com/nexthopecoming/next-gateway/internal/core/model"
)

func TestValidUsesSharedByteAndWhitespaceRules(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "ordinary", value: "model-a", want: true},
		{name: "wildcard", value: "*", want: true},
		{name: "surrounding whitespace is preserved", value: " model-a ", want: true},
		{name: "ASCII byte boundary", value: strings.Repeat("m", model.MaxBytes), want: true},
		{name: "UTF-8 byte boundary", value: strings.Repeat("界", 42) + "mm", want: true},
		{name: "empty", value: "", want: false},
		{name: "whitespace only", value: " \t\n", want: false},
		{name: "ASCII too long", value: strings.Repeat("m", model.MaxBytes+1), want: false},
		{name: "UTF-8 too long", value: strings.Repeat("界", 43), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := test.value
			if got := model.Valid(test.value); got != test.want {
				t.Fatalf("Valid() = %v，期望 %v", got, test.want)
			}
			if test.value != original {
				t.Fatal("模型校验修改了原始键")
			}
		})
	}
}
