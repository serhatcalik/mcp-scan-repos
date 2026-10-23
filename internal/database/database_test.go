package database

import (
	"context"
	"strings"
	"testing"
)

func TestOpenDoesNotExposePasswordOnInvalidURL(t *testing.T) {
	_, err := Open(context.Background(), "postgres://user:must-not-log-password@%")
	if err == nil || strings.Contains(err.Error(), "must-not-log-password") {
		t.Fatalf("unexpected connection error: %v", err)
	}
}
