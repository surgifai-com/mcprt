package proxy

import "testing"

func TestRewriteLocation(t *testing.T) {
	const server = "myserver"
	const reqHost = "127.0.0.1:9090"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "origin-relative path missing prefix gets prefix",
			in:   "/mcp/",
			want: "/myserver/mcp/",
		},
		{
			name: "origin-relative path with prefix passes through",
			in:   "/myserver/mcp/",
			want: "/myserver/mcp/",
		},
		{
			name: "origin-relative path equal to prefix passes through",
			in:   "/myserver",
			want: "/myserver",
		},
		{
			name: "absolute URL same host missing prefix gets prefix",
			in:   "http://127.0.0.1:9090/mcp/",
			want: "http://127.0.0.1:9090/myserver/mcp/",
		},
		{
			name: "absolute URL same host with prefix passes through",
			in:   "http://127.0.0.1:9090/myserver/mcp/",
			want: "http://127.0.0.1:9090/myserver/mcp/",
		},
		{
			name: "absolute URL same host preserves query and fragment",
			in:   "http://127.0.0.1:9090/mcp/?session=abc#x",
			want: "http://127.0.0.1:9090/myserver/mcp/?session=abc#x",
		},
		{
			name: "cross-host absolute URL passes through",
			in:   "https://other.example.com/mcp/",
			want: "https://other.example.com/mcp/",
		},
		{
			name: "relative path passes through (browser-resolved)",
			in:   "mcp/",
			want: "mcp/",
		},
		{
			name: "protocol-relative URL passes through",
			in:   "//evil.example.com/mcp/",
			want: "//evil.example.com/mcp/",
		},
		{
			name: "empty Location passes through",
			in:   "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteLocation(tc.in, server, reqHost)
			if got != tc.want {
				t.Errorf("rewriteLocation(%q, %q, %q) = %q, want %q",
					tc.in, server, reqHost, got, tc.want)
			}
		})
	}
}

func TestRewriteLocationDifferentReqHost(t *testing.T) {
	// When the proxy is reached via a hostname (not an IP), Location headers
	// from upstream that hard-code a different bind address (127.0.0.1:port)
	// are technically a different host and should pass through. Behavior we
	// document, not necessarily endorse.
	got := rewriteLocation("http://127.0.0.1:9090/mcp/", "myserver", "mcprt.local")
	want := "http://127.0.0.1:9090/mcp/"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
