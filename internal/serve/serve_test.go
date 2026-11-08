package serve

import (
	"io"
	"testing"
	"time"
)

func TestParseHTTP(t *testing.T) {
	o, err := Parse("w", []string{"--http", "--port", "0", "--host", "0.0.0.0"}, io.Discard)
	if err != nil || !o.HTTP || o.Port != 0 || o.Host != "0.0.0.0" {
		t.Fatalf("Parse = %+v, %v", o, err)
	}
}

// DuckDB's launcher adds flags of its own; they must not stop the worker.
func TestParseIgnoresUnknownFlags(t *testing.T) {
	o, err := Parse("w", []string{"--threaded", "--unix", "/tmp/s.sock", "--idle-timeout", "5", "stray"}, io.Discard)
	if err != nil || o.Unix != "/tmp/s.sock" || o.IdleTimeout != 5*time.Second {
		t.Fatalf("Parse = %+v, %v", o, err)
	}
}

func TestParseRejectsBothTransports(t *testing.T) {
	if _, err := Parse("w", []string{"--http", "--unix", "/tmp/s"}, io.Discard); err == nil {
		t.Fatal("want an error for --http with --unix")
	}
}
