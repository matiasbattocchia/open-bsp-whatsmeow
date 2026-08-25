package main

import "testing"

// A consumer that serves its own media has no hostname to hand us: it sends a
// relative media_url and we resolve it against the address we already deliver
// to. An absolute one (open-bsp-api's signed storage links) is left alone.
func TestResolveMediaURL(t *testing.T) {
	cases := []struct {
		name  string
		base  string
		media string
		want  string
	}{
		{
			name:  "relative resolves against an origin base",
			base:  "http://localhost:8793",
			media: "/m/eyJ9.mac",
			want:  "http://localhost:8793/m/eyJ9.mac",
		},
		{
			name:  "relative ignores the base's path — it is an origin-absolute ref",
			base:  "http://kong:8000/functions/v1",
			media: "/m/eyJ9.mac",
			want:  "http://kong:8000/m/eyJ9.mac",
		},
		{
			name:  "absolute passes through untouched",
			base:  "http://localhost:8793",
			media: "https://project.supabase.co/storage/v1/object/sign/media/x?token=y",
			want:  "https://project.supabase.co/storage/v1/object/sign/media/x?token=y",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveMediaURL(c.base, c.media)
			if err != nil {
				t.Fatalf("resolveMediaURL: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}

	if _, err := resolveMediaURL("://nonsense", "/m/x"); err == nil {
		t.Fatal("a malformed base should be reported, not dialed")
	}
}
