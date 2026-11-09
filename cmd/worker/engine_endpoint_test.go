package main

import "testing"

func TestMapEngineEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		engine   string
		igpu     string
		want     string
	}{
		{
			name:     "brain endpoint goes to the brain",
			endpoint: "http://localhost:11434/v1",
			engine:   "http://engine:11434",
			igpu:     "http://jarvis-engine-igpu:11435",
			want:     "http://engine:11434",
		},
		{
			name:     "igpu endpoint goes to the igpu container, not the brain",
			endpoint: "http://localhost:11435/v1",
			engine:   "http://engine:11434",
			igpu:     "http://jarvis-engine-igpu:11435",
			want:     "http://jarvis-engine-igpu:11435",
		},
		{
			name:     "igpu env override wins",
			endpoint: "http://localhost:11435/v1",
			engine:   "http://engine:11434",
			igpu:     "http://localhost:11435",
			want:     "http://localhost:11435",
		},
		{
			name:     "other local ports stay on the brain (ft serve 11436)",
			endpoint: "http://localhost:11436/v1",
			engine:   "http://engine:11434",
			igpu:     "http://jarvis-engine-igpu:11435",
			want:     "http://engine:11434",
		},
		{
			name:     "host run (no engine env) passes endpoints through",
			endpoint: "http://localhost:11435/v1",
			engine:   "",
			igpu:     "http://jarvis-engine-igpu:11435",
			want:     "http://localhost:11435/v1",
		},
		{
			name:     "endpoint without a port goes to the brain",
			endpoint: "http://engine",
			engine:   "http://engine:11434",
			igpu:     "http://jarvis-engine-igpu:11435",
			want:     "http://engine:11434",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapEngineEndpoint(tc.endpoint, tc.engine, tc.igpu); got != tc.want {
				t.Errorf("mapEngineEndpoint(%q, %q, %q) = %q, want %q",
					tc.endpoint, tc.engine, tc.igpu, got, tc.want)
			}
		})
	}
}
