package runner

import "testing"

func TestCheckTarget(t *testing.T) {
	for _, ok := range []string{"http://localhost:8080", "http://127.0.0.1:3000/api", "http://[::1]:80", "https://10.0.0.5", "http://192.168.1.20:8000", "http://app.localhost"} {
		if _, err := CheckTarget(ok, false); err != nil {
			t.Errorf("%s should be allowed: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://8.8.8.8", "ftp://localhost", "localhost:8080", "http://"} {
		if _, err := CheckTarget(bad, false); err == nil {
			t.Errorf("%s should be refused", bad)
		}
	}
	if _, err := CheckTarget("http://8.8.8.8", true); err != nil {
		t.Errorf("--allow-remote should allow public hosts: %v", err)
	}
}
