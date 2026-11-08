package realtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A client that does not drain its queue is closed with 4000 instead of
// letting memory grow (plan 5.5: bounded queues, slow client => resync).
func TestConn_SlowConsumerIsClosed(t *testing.T) {
	t.Parallel()
	ready := make(chan *conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		c := &conn{ws: ws, out: make(chan outMsg, 1), snapSig: make(chan struct{}, 1), ctx: ctx, cancel: cancel}
		ready <- c
		<-ctx.Done()
	}))
	defer srv.Close()
	client, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	c := <-ready
	c.send([]byte(`{"v":1}`)) // fills the queue (no writer running)
	c.send([]byte(`{"v":1}`)) // overflow → close 4000
	if !c.closing.Load() {
		t.Fatal("overflow must start closing the connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err = client.Read(ctx)
	if code := websocket.CloseStatus(err); code != CloseSlowConsumer {
		t.Fatalf("close code %d (%v)", code, err)
	}
}

// Snapshots are coalesced: only the newest pending one is written.
func TestConn_SnapshotCoalescing(t *testing.T) {
	t.Parallel()
	c := &conn{out: make(chan outMsg, 4), snapSig: make(chan struct{}, 1)}
	c.pushSnapshot(5, []byte("v5"))
	c.pushSnapshot(7, []byte("v7"))
	c.pushSnapshot(6, []byte("v6")) // older than the last pushed: ignored
	if got := string(*c.snap.Load()); got != "v7" {
		t.Fatalf("pending snapshot %q", got)
	}
	if len(c.snapSig) != 1 {
		t.Fatal("one wake-up for many snapshots")
	}
}

func TestEncode(t *testing.T) {
	t.Parallel()
	b := encode(TypeError, Envelope{RequestID: "r"}, map[string]any{"error": map[string]string{"code": "X"}})
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil || e.V != 1 || e.Type != TypeError || e.RequestID != "r" || !strings.Contains(string(e.Payload), `"X"`) {
		t.Fatalf("envelope %s", b)
	}
}

func FuzzEnvelopeDecode(f *testing.F) {
	f.Add([]byte(`{"v":1,"type":"auth","payload":{"accessToken":"x"}}`))
	f.Add([]byte(`{"v":1,"type":"command","payload":{"operation":"vote.submit","payload":{}}}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var e Envelope
		if json.Unmarshal(data, &e) != nil {
			return
		}
		var p AuthPayload
		_ = json.Unmarshal(e.Payload, &p) // must never panic
	})
}
