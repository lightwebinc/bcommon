package wirewallet

import "testing"

func TestCheckURLIsLoopbackOnly(t *testing.T) {
	for _, ok := range []string{"http://127.0.0.1:3301", "http://localhost:3301", "http://[::1]:3301"} {
		if err := CheckURL(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://192.0.2.10:3301", "http://wallet.example.com", "unix:///tmp/w.sock", "ftp://127.0.0.1"} {
		if err := CheckURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
