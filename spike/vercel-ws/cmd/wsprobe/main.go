// Command wsprobe validates the spike server end to end and prints verifiable
// evidence: HTTP status codes, the 101 handshake, echo round trips, two
// simultaneous clients, reconnections, Origin rejection, frame limit and,
// optionally, a long-held session that measures the platform's effective
// maximum connection duration.
//
//	go run ./cmd/wsprobe -base https://<preview>.vercel.app
//	go run ./cmd/wsprobe -base https://<preview>.vercel.app -hold 16m
//
// A Vercel "Protection Bypass for Automation" secret can be supplied through
// the VERCEL_AUTOMATION_BYPASS_SECRET environment variable (never a flag, so
// it does not end up in shell history). It is sent as a header, never printed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type probe struct {
	base     string
	wsURL    string
	bypass   string
	failures int
}

func main() {
	base := flag.String("base", "http://localhost:3000", "base URL of the deployment")
	reconnects := flag.Int("reconnects", 3, "number of close+reconnect cycles")
	hold := flag.Duration("hold", 0, "keep one connection alive this long, reconnecting on platform close (0 = skip)")
	allowedOrigin := flag.String("allowed-origin", "", "an origin configured in ALLOWED_ORIGINS, to test browser-like acceptance")
	flag.Parse()

	p := &probe{base: strings.TrimRight(*base, "/"), bypass: os.Getenv("VERCEL_AUTOMATION_BYPASS_SECRET")}
	p.wsURL = "ws" + strings.TrimPrefix(p.base, "http") + "/ws"
	fmt.Printf("# wsprobe %s base=%s ws=%s bypass=%t\n", time.Now().UTC().Format(time.RFC3339), p.base, p.wsURL, p.bypass != "")

	p.checkHealth()
	p.checkEcho()
	p.checkTwoClients()
	p.checkReconnect(*reconnects)
	p.checkOriginRejected()
	if *allowedOrigin != "" {
		p.checkOriginAllowed(*allowedOrigin)
	}
	p.checkFrameLimit()
	if *hold > 0 {
		p.holdSession(*hold)
	}
	fmt.Printf("# done failures=%d\n", p.failures)
	if p.failures > 0 {
		os.Exit(1)
	}
}

func (p *probe) result(name string, ok bool, format string, args ...any) {
	status := "PASS"
	if !ok {
		status = "FAIL"
		p.failures++
	}
	fmt.Printf("%s %-22s %s\n", status, name, fmt.Sprintf(format, args...))
}

func (p *probe) header(origin string) http.Header {
	h := http.Header{}
	if p.bypass != "" {
		h.Set("x-vercel-protection-bypass", p.bypass)
	}
	if origin != "" {
		h.Set("Origin", origin)
	}
	return h
}

func (p *probe) checkHealth() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/healthz", nil)
	req.Header = p.header("")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.result("http.health", false, "error=%v", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	p.result("http.health", resp.StatusCode == http.StatusOK, "status=%d latency=%s x-vercel-id=%q body=%s",
		resp.StatusCode, time.Since(start).Round(time.Millisecond), resp.Header.Get("x-vercel-id"), strings.TrimSpace(string(body)))
}

type serverMsg struct {
	Type       string `json:"type"`
	Data       string `json:"data"`
	InstanceID string `json:"instanceId"`
	ConnID     string `json:"connId"`
	Delivered  int    `json:"delivered"`
	From       string `json:"from"`
}

func (p *probe) dial(ctx context.Context, origin string) (*websocket.Conn, *http.Response, serverMsg, error) {
	c, resp, err := websocket.Dial(ctx, p.wsURL, &websocket.DialOptions{HTTPHeader: p.header(origin)})
	if err != nil {
		return nil, resp, serverMsg{}, err
	}
	c.SetReadLimit(1 << 20)
	hello, err := readMsg(ctx, c)
	if err != nil {
		c.CloseNow()
		return nil, resp, serverMsg{}, fmt.Errorf("reading hello: %w", err)
	}
	return c, resp, hello, nil
}

func readMsg(ctx context.Context, c *websocket.Conn) (serverMsg, error) {
	_, b, err := c.Read(ctx)
	if err != nil {
		return serverMsg{}, err
	}
	var m serverMsg
	if err := json.Unmarshal(b, &m); err != nil {
		return serverMsg{}, fmt.Errorf("decoding %q: %w", b, err)
	}
	return m, nil
}

func echo(ctx context.Context, c *websocket.Conn, text string) (serverMsg, time.Duration, error) {
	start := time.Now()
	if err := c.Write(ctx, websocket.MessageText, []byte(text)); err != nil {
		return serverMsg{}, 0, err
	}
	m, err := readMsg(ctx, c)
	return m, time.Since(start), err
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func (p *probe) checkEcho() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, resp, hello, err := p.dial(ctx, "")
	if err != nil {
		p.result("ws.handshake", false, "status=%d error=%v", statusOf(resp), err)
		return
	}
	defer c.CloseNow()
	p.result("ws.handshake", statusOf(resp) == http.StatusSwitchingProtocols,
		"status=%d upgrade=%q instance=%s conn=%s x-vercel-id=%q",
		statusOf(resp), resp.Header.Get("Upgrade"), hello.InstanceID, hello.ConnID, resp.Header.Get("x-vercel-id"))

	const unicode = "hola ñandú — 🚀 漢字 \"quotes\""
	m, rtt, err := echo(ctx, c, unicode)
	p.result("ws.echo.unicode", err == nil && m.Type == "echo" && m.Data == unicode, "rtt=%s got=%q err=%v", rtt.Round(time.Millisecond), m.Data, err)

	var worst time.Duration
	okAll := true
	for i := range 10 {
		want := fmt.Sprintf("msg-%d", i)
		m, rtt, err := echo(ctx, c, want)
		okAll = okAll && err == nil && m.Data == want
		worst = max(worst, rtt)
	}
	p.result("ws.echo.sequence", okAll, "10 messages in order, worst rtt=%s", worst.Round(time.Millisecond))

	// coder/websocket processes the pong (and the close handshake) only while
	// a Read is in progress, so keep a reader running from here on.
	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}()
	pingStart := time.Now()
	pingErr := c.Ping(ctx)
	p.result("ws.ping", pingErr == nil, "rtt=%s err=%v", time.Since(pingStart).Round(time.Millisecond), pingErr)

	err = c.Close(websocket.StatusNormalClosure, "probe done")
	p.result("ws.close.normal", err == nil, "err=%v", err)
}

func (p *probe) checkTwoClients() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, _, ha, errA := p.dial(ctx, "")
	b, _, hb, errB := p.dial(ctx, "")
	if errA != nil || errB != nil {
		p.result("ws.two-clients", false, "errA=%v errB=%v", errA, errB)
		return
	}
	defer a.CloseNow()
	defer b.CloseNow()

	ma, _, e1 := echo(ctx, a, "from-a")
	mb, _, e2 := echo(ctx, b, "from-b")
	p.result("ws.two-clients", e1 == nil && e2 == nil && ma.Data == "from-a" && mb.Data == "from-b",
		"a.instance=%s b.instance=%s sameInstance=%t", ha.InstanceID, hb.InstanceID, ha.InstanceID == hb.InstanceID)

	// Fan-out only reaches sockets held by the same instance (no shared memory).
	if err := a.Write(ctx, websocket.MessageText, []byte("broadcast:hi-b")); err != nil {
		p.result("ws.local-broadcast", false, "write err=%v", err)
		return
	}
	gotAck, gotOnB := false, false
	deadline, cancelB := context.WithTimeout(ctx, 5*time.Second)
	defer cancelB()
	for !gotAck {
		m, err := readMsg(deadline, a)
		if err != nil {
			break
		}
		gotAck = gotAck || m.Type == "broadcast.ack"
	}
	if ha.InstanceID == hb.InstanceID {
		m, err := readMsg(deadline, b)
		gotOnB = err == nil && m.Type == "broadcast" && m.Data == "hi-b"
		p.result("ws.local-broadcast", gotAck && gotOnB, "same instance: b received=%t", gotOnB)
	} else {
		p.result("ws.local-broadcast", gotAck, "different instances: local fan-out cannot reach b (expected; cross-instance needs PG polling)")
	}
	_ = a.Close(websocket.StatusNormalClosure, "")
	_ = b.Close(websocket.StatusNormalClosure, "")
}

func (p *probe) checkReconnect(n int) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(n+1)*30*time.Second)
	defer cancel()
	instances := map[string]int{}
	ok := true
	for i := range n {
		c, resp, hello, err := p.dial(ctx, "")
		if err != nil {
			ok = false
			fmt.Printf("  reconnect %d: status=%d err=%v\n", i+1, statusOf(resp), err)
			continue
		}
		m, rtt, err := echo(ctx, c, fmt.Sprintf("cycle-%d", i))
		ok = ok && err == nil && m.Type == "echo"
		instances[hello.InstanceID]++
		fmt.Printf("  reconnect %d: instance=%s rtt=%s\n", i+1, hello.InstanceID, rtt.Round(time.Millisecond))
		_ = c.Close(websocket.StatusNormalClosure, "cycle")
	}
	p.result("ws.reconnect", ok, "%d cycles, instances seen=%v", n, instances)
}

func (p *probe) checkOriginRejected() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, resp, _, err := p.dial(ctx, "https://evil.example")
	if c != nil {
		c.CloseNow()
	}
	p.result("ws.origin.rejected", err != nil && statusOf(resp) == http.StatusForbidden, "status=%d", statusOf(resp))
}

func (p *probe) checkOriginAllowed(origin string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, resp, _, err := p.dial(ctx, origin)
	if c != nil {
		defer c.CloseNow()
	}
	p.result("ws.origin.allowed", err == nil && statusOf(resp) == http.StatusSwitchingProtocols, "origin=%s status=%d err=%v", origin, statusOf(resp), err)
}

func (p *probe) checkFrameLimit() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, _, _, err := p.dial(ctx, "")
	if err != nil {
		p.result("ws.frame-limit", false, "dial err=%v", err)
		return
	}
	defer c.CloseNow()
	_ = c.Write(ctx, websocket.MessageText, []byte(strings.Repeat("x", 20<<10)))
	_, _, err = c.Read(ctx)
	code := websocket.CloseStatus(err)
	p.result("ws.frame-limit", code == websocket.StatusMessageTooBig, "20KiB frame -> close=%d (%v)", code, code)
}

// holdSession keeps a connection open, pinging every 20 s; whenever the
// platform closes it, it records the effective lifetime and reconnects.
func (p *probe) holdSession(total time.Duration) {
	end := time.Now().Add(total)
	cycles := 0
	var lifetimes []time.Duration
	for time.Now().Before(end) {
		ctx, cancel := context.WithDeadline(context.Background(), end)
		c, resp, hello, err := p.dial(ctx, "")
		if err != nil {
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				break
			}
			p.result("hold.dial", false, "status=%d err=%v", statusOf(resp), err)
			time.Sleep(2 * time.Second)
			continue
		}
		opened := time.Now()
		cycles++
		fmt.Printf("  hold: connection %d open instance=%s at %s\n", cycles, hello.InstanceID, opened.UTC().Format(time.RFC3339))

		readErr := make(chan error, 1)
		go func() {
			for {
				if _, _, err := c.Read(ctx); err != nil {
					readErr <- err
					return
				}
			}
		}()
		ticker := time.NewTicker(20 * time.Second)
		var closeErr error
	loop:
		for {
			select {
			case err := <-readErr:
				closeErr = err
				break loop
			case <-ticker.C:
				if err := c.Write(ctx, websocket.MessageText, []byte("info")); err != nil {
					closeErr = err
					break loop
				}
			case <-ctx.Done():
				break loop
			}
		}
		ticker.Stop()
		life := time.Since(opened)
		c.CloseNow()
		cancel()
		if ctx.Err() != nil && closeErr == nil {
			fmt.Printf("  hold: connection %d still open at end of hold (%s)\n", cycles, life.Round(time.Second))
			break
		}
		lifetimes = append(lifetimes, life)
		fmt.Printf("  hold: connection %d closed by remote after %s close=%d err=%v\n",
			cycles, life.Round(time.Second), websocket.CloseStatus(closeErr), closeErr)
	}
	p.result("hold.session", cycles > 0, "held=%s connections=%d platform-closes=%d lifetimes=%v", total, cycles, len(lifetimes), lifetimes)
}
