package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	writeWait      = 10 * time.Second
	idleTimeout    = 90 * time.Second // ohne eingehende Zeile wird getrennt
	pingPeriod     = 30 * time.Second
	authTimeout    = 30 * time.Second
	maxLineSize    = 64 * 1024
	sendQueueSize  = 256
	msgBurst       = 6
	msgBurstWindow = 4 * time.Second
)

// Frame ist ein eingehendes Paket (eine JSON-Zeile).
type Frame struct {
	Op       string `json:"op"`
	Ref      string `json:"ref,omitempty"`
	Token    string `json:"token,omitempty"`
	Username string `json:"username,omitempty"`
	Channel  string `json:"channel,omitempty"`
	Target   string `json:"target,omitempty"`
	Text     string `json:"text,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Topic    string `json:"topic,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Before   string `json:"before,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Edit     string `json:"edit,omitempty"`
}

// connWrapper bietet dieselbe kleine Schnittstelle wie vorher die
// WebSocket-Verbindung (RemoteAddr, Close).
type connWrapper struct {
	net.Conn
	once sync.Once
}

func (c *connWrapper) Close() error {
	var err error
	c.once.Do(func() { err = c.Conn.Close() })
	return err
}

type Client struct {
	hub    *Hub
	conn   *connWrapper
	send   chan []byte
	secure bool

	user    string
	profile *Profile
	authed  atomic.Bool
	recent  []time.Time
}

func newClient(h *Hub, conn net.Conn, secure bool) *Client {
	return &Client{hub: h, conn: &connWrapper{Conn: conn}, send: make(chan []byte, sendQueueSize), secure: secure}
}

func (c *Client) emit(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	select {
	case c.send <- data:
	default:
		go c.conn.Close() // Client kommt nicht hinterher
	}
}

func (c *Client) fail(ref, code, msg string) {
	c.emit(map[string]any{"op": "error", "ref": ref, "code": code, "message": msg})
}

func (c *Client) readPump() {
	defer func() {
		c.hub.disconnect(c)
		c.conn.Close()
	}()

	c.emit(map[string]any{
		"op":            "hello",
		"protocol":      "mirc/1",
		"server":        map[string]string{"name": c.hub.cfg.Name, "motd": c.hub.cfg.MOTD},
		"sessionServer": c.hub.cfg.PublicSession,
		"tls":           c.secure,
	})

	authTimer := time.AfterFunc(authTimeout, func() {
		if !c.authed.Load() {
			c.conn.Close()
		}
	})
	defer authTimer.Stop()

	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 4096), maxLineSize)
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(idleTimeout))
		if !sc.Scan() {
			if err := sc.Err(); errors.Is(err, bufio.ErrTooLong) {
				c.fail("", "bad_frame", "Zeile zu lang")
			}
			return
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			c.fail("", "bad_frame", "Ungültiges JSON")
			continue
		}
		if f.Op == "pong" {
			continue
		}
		if !c.authed.Load() {
			if f.Op == "ping" {
				c.emit(map[string]any{"op": "pong", "ref": f.Ref})
				continue
			}
			if f.Op != "auth" {
				c.fail(f.Ref, "not_authenticated", "Bitte zuerst mit einem Token anmelden")
				continue
			}
			c.handleAuth(f)
			continue
		}
		c.hub.handle(c, f)
	}
}

func (c *Client) handleAuth(f Frame) {
	if len(f.Token) != 64 {
		c.fail(f.Ref, "auth_failed", "Ungültiger Token")
		return
	}
	p, err := verifyToken(c.hub.cfg, f.Token)
	if err != nil {
		msg := err.Error()
		var ve *verifyError
		if errors.As(err, &ve) && ve.issuedFor != "" {
			msg += " (Token für " + ve.issuedFor + ", dieser Server akzeptiert nur die konfigurierten Adressen)"
		}
		log.Printf("Anmeldung abgelehnt (%s): %s", c.conn.RemoteAddr(), msg)
		c.fail(f.Ref, "auth_failed", msg)
		return
	}
	c.hub.connect(c, p)
}

func (c *Client) floodCheck() bool {
	now := time.Now()
	kept := c.recent[:0]
	for _, t := range c.recent {
		if now.Sub(t) < msgBurstWindow {
			kept = append(kept, t)
		}
	}
	c.recent = kept
	if len(c.recent) >= msgBurst {
		return false
	}
	c.recent = append(c.recent, now)
	return true
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	w := bufio.NewWriter(c.conn)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	write := func(data []byte) bool {
		_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
		if _, err := w.Write(data); err != nil {
			return false
		}
		if err := w.WriteByte('\n'); err != nil {
			return false
		}
		return true
	}
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				_ = w.Flush()
				return
			}
			if !write(msg) {
				return
			}
			// Weitere wartende Pakete gleich mitschicken, dann einmal flushen.
			for n := len(c.send); n > 0; n-- {
				if !write(<-c.send) {
					return
				}
			}
			if w.Flush() != nil {
				return
			}
		case <-ticker.C:
			if !write([]byte(`{"op":"ping"}`)) || w.Flush() != nil {
				return
			}
		}
	}
}
