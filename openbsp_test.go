package main

import (
	"testing"
	"time"
)

// The upload deadline doubles as how long one medium may hold up a session's
// event stream, so it has to grow with the payload and then stop growing.
func TestMediaUploadTimeoutScalesWithSizeAndIsBounded(t *testing.T) {
	const mb = 1000 * 1000

	cases := []struct {
		name string
		size int
		want time.Duration
	}{
		{"an empty body still waits out a cold start", 0, 30 * time.Second},
		{"a 1 MB image", 1 * mb, 40 * time.Second},
		{"a 5 MB video", 5 * mb, 80 * time.Second},
		{"the largest upload the webhook accepts", 50 * mb, 5 * time.Minute},
	}
	for _, c := range cases {
		if got := mediaUploadTimeout(c.size); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}

	// Monotonic: a bigger upload never gets less time than a smaller one.
	prev := time.Duration(0)
	for size := 0; size <= 60*mb; size += mb {
		got := mediaUploadTimeout(size)
		if got < prev {
			t.Fatalf("timeout shrank at %d MB: %v after %v", size/mb, got, prev)
		}
		prev = got
	}
}
