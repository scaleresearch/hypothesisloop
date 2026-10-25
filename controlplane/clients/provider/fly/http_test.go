package fly

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blackhole forwards each connection to target until cut, after which the connections it
// already holds stay open and carry nothing either way, as a dead network path does. A
// connection made after the cut is forwarded.
type blackhole struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	cut    []*atomic.Bool
}

func newBlackhole(t *testing.T, target string) *blackhole {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &blackhole{ln: ln, target: target}
	t.Cleanup(func() { ln.Close() })
	go b.serve()
	return b
}

func (b *blackhole) serve() {
	for {
		client, err := b.ln.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", b.target)
		if err != nil {
			client.Close()
			continue
		}
		dead := &atomic.Bool{}
		b.mu.Lock()
		b.cut = append(b.cut, dead)
		b.mu.Unlock()
		go pipe(client, server, dead)
		go pipe(server, client, dead)
	}
}

func pipe(from, to net.Conn, dead *atomic.Bool) {
	buf := make([]byte, 32<<10)
	for {
		n, err := from.Read(buf)
		if err != nil {
			return
		}
		if !dead.Load() {
			if _, err := to.Write(buf[:n]); err != nil {
				return
			}
		}
	}
}

func (b *blackhole) cutAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, dead := range b.cut {
		dead.Store(true)
	}
}

func TestHTTPClientLeavesADeadConnection(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Proto)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	hole := newBlackhole(t, srv.Listener.Addr().String())

	client := newHTTPClient(time.Second, 200*time.Millisecond, 200*time.Millisecond)
	client.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	url := "https://" + hole.ln.Addr().String() + "/"
	get := func() (string, error) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return string(body), err
	}

	if proto, err := get(); err != nil || proto != "HTTP/2.0" {
		t.Fatalf("first request: %q, %v; want HTTP/2.0", proto, err)
	}
	hole.cutAll()
	if _, err := get(); err == nil {
		t.Fatal("a request on the cut connection succeeded")
	}
	var err error
	for range 3 {
		if _, err = get(); err == nil {
			return
		}
	}
	t.Fatalf("every request after the cut failed, the last with %v", err)
}
