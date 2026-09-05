package credtest

import "testing"

func TestRemoteHTTPURLRecognizesPlaintextEndpoint(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  bool
	}{
		{name: "https", value: "https://issuer.example", want: true},
		{name: "plaintext http", value: "http://issuer.example", want: true},
		{name: "non-http scheme", value: "file:///var/run/token"},
		{name: "missing host", value: "https:///issuer"},
		{name: "non-string", value: 443},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := remoteHTTPURL(tc.value)
			if got != tc.want {
				t.Errorf("remoteHTTPURL(%v) recognized = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
