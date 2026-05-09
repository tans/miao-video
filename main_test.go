package main

import (
	"context"
	"errors"
	"net"
	"testing"
)

type testNetError struct{}

func (e testNetError) Error() string   { return "temporary network error" }
func (e testNetError) Timeout() bool   { return true }
func (e testNetError) Temporary() bool { return true }

func TestIsRetryableProcessingError(t *testing.T) {
	t.Run("http status retryable", func(t *testing.T) {
		if !isRetryableProcessingError(&httpStatusError{stage: "download", code: 500}) {
			t.Fatal("expected 500 to be retryable")
		}
	})

	t.Run("http status not retryable", func(t *testing.T) {
		if isRetryableProcessingError(&httpStatusError{stage: "download", code: 404}) {
			t.Fatal("expected 404 to be non-retryable")
		}
	})

	t.Run("network timeout retryable", func(t *testing.T) {
		var err error = testNetError{}
		if !isRetryableProcessingError(err) {
			t.Fatal("expected timeout net error to be retryable")
		}
	})

	t.Run("plain error not retryable", func(t *testing.T) {
		if isRetryableProcessingError(errors.New("bad input")) {
			t.Fatal("expected plain error to be non-retryable")
		}
	})

	t.Run("deadline retryable", func(t *testing.T) {
		if !isRetryableProcessingError(context.DeadlineExceeded) {
			t.Fatal("expected deadline exceeded to be retryable")
		}
	})
}

var _ net.Error = testNetError{}
