package hetzner

import (
	"context"
	"encoding/json"
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

// Every primary IP counts against the project's limit, IPv6 as much as
// IPv4, and nothing here reaches a box over IPv6; so a server is created
// with its IPv4 alone.
func TestCreateServerAsksForIPv4Only(t *testing.T) {
	var publicNet map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/server_types":
			w.Write([]byte(`{"server_types":[{"id":1,"name":"cx22","architecture":"x86"}]}`))
		case r.Method == "GET" && r.URL.Path == "/images":
			w.Write([]byte(`{"images":[{"id":2,"name":"ubuntu-24.04","architecture":"x86"}]}`))
		case r.Method == "GET" && r.URL.Path == "/locations":
			w.Write([]byte(`{"locations":[{"id":3,"name":"fsn1"}]}`))
		case r.Method == "GET" && r.URL.Path == "/firewalls":
			w.Write([]byte(`{"firewalls":[{"id":4,"name":"gobank-x"}]}`))
		case r.Method == "POST" && r.URL.Path == "/servers":
			var body struct {
				PublicNet map[string]any `json:"public_net"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			publicNet = body.PublicNet
			// Refuse it: only the request is under test.
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"error":{"code":"invalid_input","message":"stop"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	c := &Cloud{c: hcloud.NewClient(hcloud.WithToken("t"), hcloud.WithEndpoint(ts.URL), hcloud.WithRetryOpts(hcloud.RetryOpts{MaxRetries: 0}))}
	c.CreateServer(context.Background(), deploy.CreateSpec{Name: "gobank-x", Type: "cx22", Image: "ubuntu-24.04", Location: "fsn1", Firewall: "gobank-x"})
	if publicNet["enable_ipv4"] != true || publicNet["enable_ipv6"] != false {
		t.Errorf("public_net = %v; want IPv4 only", publicNet)
	}
}
