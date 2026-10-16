package hetzner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
)

// Two ups at once both find no network and both create it; the provider
// refuses the second as a name already used. That is the network
// existing, which is all EnsureNetwork promises.
func TestEnsureNetworkTakesALostRaceAsTheNetworkExisting(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/networks":
			// The look finds nothing: the other up has not created it yet.
			w.Write([]byte(`{"networks":[],"meta":{"pagination":{"page":1,"per_page":25,"total_entries":0}}}`))
		case r.Method == "POST" && r.URL.Path == "/networks":
			posts++
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":{"code":"uniqueness_error","message":"name is already used"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	c := &Cloud{c: hcloud.NewClient(hcloud.WithToken("t"), hcloud.WithEndpoint(ts.URL), hcloud.WithRetryOpts(hcloud.RetryOpts{MaxRetries: 0}))}
	if err := c.EnsureNetwork(context.Background(), deploy.PrivateNetwork); err != nil {
		t.Errorf("err = %v; want a lost race taken as the network existing", err)
	}
	if posts != 1 {
		t.Errorf("posts = %d", posts)
	}
}
