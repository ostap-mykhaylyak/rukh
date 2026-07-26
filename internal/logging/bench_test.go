package logging

import (
	"testing"
	"time"
)

func BenchmarkAccessLine(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Access.Info("request",
			"host", "petralito.ostap.dev",
			"method", "GET",
			"path", "/wp-content/plugins/woocommerce/assets/js/frontend/add-to-cart.min.js?ver=10.9.4",
			"status", 200,
			"bytes", int64(1444),
			"duration_ms", 0.977,
			"origin_ms", 0.804,
			"kind", "script",
			"speculative", false,
			"client", "89.46.225.7",
			"proto", "HTTP/1.1",
			"hints", 0,
		)
	}
	b.StopTimer()
	_ = time.Now()
}
