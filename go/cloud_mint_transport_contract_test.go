package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMintControlPlaneUsesHTTPForEitherUpstreamTransport(t *testing.T) {
	for _, upstream := range []string{"sse", "websocket"} {
		t.Run(upstream, func(t *testing.T) {
			type requestView struct{ method, transport, upgrade string }
			observed := make(chan requestView, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed <- requestView{r.Method, r.Header.Get("X-Mint-Transport"), r.Header.Get("Upgrade")}
				result := cloudTestResult(time.Now().Truncate(time.Second), "gpt-6-sol")
				result.Transport = upstream
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(result); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			cfg := defaultCloudMintConfig()
			cfg.URL, cfg.Transport = server.URL, upstream
			entry, err := requestCloudMint(context.Background(), cloudMintWork{
				cfg: cfg, model: "gpt-6-sol", key: "fixture-relay-key",
				creds: cloudMintCredentials{AccessToken: "fixture-access-token"},
			})
			if err != nil {
				t.Fatal(err)
			}
			seen := <-observed
			if seen.method != http.MethodPost || seen.transport != upstream || seen.upgrade != "" {
				t.Fatalf("unexpected control transport: %+v", seen)
			}
			if len(entry.Ticket) != cfg.TicketLength {
				t.Fatal("validated ticket missing")
			}
		})
	}
}
