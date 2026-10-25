package cluster

import (
	"strings"
	"testing"
)

func TestNewRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "empty endpoint",
			cfg:     Config{Name: "c"},
			wantErr: "endpoint is empty",
		},
		{
			name:    "unsupported scheme",
			cfg:     Config{Name: "c", Endpoint: "http://localhost:2375"},
			wantErr: `unsupported endpoint scheme "http"`,
		},
		{
			name:    "unix with tls",
			cfg:     Config{Name: "c", Endpoint: "unix:///var/run/docker.sock", CAFile: "ca.pem"},
			wantErr: "TLS is not supported for unix endpoints",
		},
		{
			name:    "tcp without tls",
			cfg:     Config{Name: "c", Endpoint: "tcp://10.0.0.1:2376"},
			wantErr: "tcp endpoint requires CA, cert and key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewUnix(t *testing.T) {
	cli, err := New(Config{Name: "local", Endpoint: "unix:///var/run/docker.sock"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := cli.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}
