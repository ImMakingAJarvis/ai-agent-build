package dashboard

import "testing"

func TestVoiceChatURLFromHost(t *testing.T) {
	cases := []struct{ in, want string }{
		// Remote LAN hosts get the HTTPS link (mic needs a secure context).
		{"111.1.2.27:8080", "https://111.1.2.27:8443/voice/chat"},
		{"111.1.2.27", "https://111.1.2.27:8443/voice/chat"},
		{"HomeAI.local:8080", "https://HomeAI.local:8443/voice/chat"},
		// localhost is a secure context: plain HTTP, no cert warning.
		{"localhost:8080", "http://localhost:8100/voice/chat"},
		{"localhost", "http://localhost:8100/voice/chat"},
		{"127.0.0.1:8080", "http://127.0.0.1:8100/voice/chat"},
		{"[::1]:8080", "http://[::1]:8100/voice/chat"},
	}
	for _, c := range cases {
		if got := voiceChatURLFromHost(c.in); got != c.want {
			t.Errorf("voiceChatURLFromHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
