package ingest

import "testing"

func TestValidateFetchURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://example.com/a.jpg":         true,
		"http://93.184.216.34/":             true,
		"ftp://example.com/":                false,
		"https://user@example.com/":         false,
		"http://localhost:8080/":            false,
		"http://api.localhost/":             false,
		"http://127.0.0.1/":                 false,
		"http://10.1.2.3/":                  false,
		"http://169.254.169.254/latest/":    false,
		"http://100.64.0.1/":                false,
		"http://0.0.0.0/":                   false,
		"http://[::1]/":                     false,
		"http://[::ffff:127.0.0.1]/":        false,
		"http://[fd00::1]/":                 false,
		"http://[64:ff9b::a00:1]/":          false,
		"http://[2606:4700:4700::1111]/dns": true,
	} {
		if err := validateFetchURL(raw); (err == nil) != ok {
			t.Errorf("validateFetchURL(%q) err=%v, want ok=%v", raw, err, ok)
		}
	}
}
