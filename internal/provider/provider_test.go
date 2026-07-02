package provider

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsAuthError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("something else"), false},
		{"sentinel direct", ErrAuthFailure, true},
		{"sentinel wrapped", fmt.Errorf("op: %w", ErrAuthFailure), true},
		{"string 401", errors.New("request failed: 401 Unauthorized"), true},
		{"string 403", errors.New("403 Forbidden"), true},
		{"string unauthorized", errors.New("UNAUTHORIZED"), true},
		{"string forbidden", errors.New("forbidden by policy"), true},
		{"unrelated number", errors.New("500 internal server error"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAuthError(tt.err); got != tt.want {
				t.Errorf("IsAuthError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
