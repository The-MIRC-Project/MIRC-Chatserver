package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const historyKeep = 1000 // Nachrichten pro Channel/DM im Speicher

// Message ist eine Chatnachricht in einem Channel oder einer DM.
type Message struct {
	ID     string `json:"id"`
	Target string `json:"target"` // "#channel" oder Benutzername des Empfängers (DM)
	From   string `json:"from"`   // Benutzername des Absenders
	Text   string `json:"text"`
	Kind   string `json:"kind"` // "text" | "action" | "system"
	TS     int64  `json:"ts"`   // Unix-Millisekunden
	Edited int64  `json:"edited,omitempty"`
}

// logRecord ist eine Zeile im Verlaufs-Logfile (append-only).
type logRecord struct {
	Op  string   `json:"op"` // "msg" | "edit" | "delete"
	Msg *Message `json:"msg,omitempty"`
	ID  string   `json:"id,omitempty"`
	Txt string   `json:"text,omitempty"`
	TS  int64    `json:"ts,omitempty"`
}

type conversation struct {
	msgs   []*Message
	file   *os.File
	loaded bool
}

// History speichert Verläufe als JSON-Lines-Dateien pro Unterhaltung und
// hält die letzten Nachrichten im Speicher.
type History struct {
	mu    sync.Mutex
	dir   string
	convs map[string]*conversation
	seq   int64
	last  int64
}

func newHistory(dir string) (*History, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &History{dir: dir, convs: map[string]*conversation{}}, nil
}

// nextID liefert monoton steigende, zeitlich sortierbare IDs.
func (h *History) nextID() string {
	now := time.Now().UnixMilli()
	if now == h.last {
		h.seq++
	} else {
		h.seq, h.last = 0, now
	}
	return strconv.FormatInt(now*1000+h.seq, 10)
}

func (h *History) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(h.dir, hex.EncodeToString(sum[:12])+".jsonl")
}

func (h *History) get(key string) *conversation {
	c := h.convs[key]
	if c != nil && c.loaded {
		return c
	}
	c = &conversation{loaded: true}
	h.convs[key] = c
	p := h.path(key)
	if f, err := os.Open(p); err == nil {
		byID := map[string]*Message{}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			var r logRecord
			if json.Unmarshal(sc.Bytes(), &r) != nil {
				continue
			}
			switch r.Op {
			case "msg":
				if r.Msg != nil {
					c.msgs = append(c.msgs, r.Msg)
					byID[r.Msg.ID] = r.Msg
				}
			case "edit":
				if m := byID[r.ID]; m != nil {
					m.Text, m.Edited = r.Txt, r.TS
				}
			case "delete":
				if m := byID[r.ID]; m != nil {
					m.Kind = "deleted"
					m.Text = ""
				}
			}
			if len(c.msgs) > historyKeep*2 {
				c.msgs = c.msgs[len(c.msgs)-historyKeep:]
			}
		}
		f.Close()
		c.msgs = compact(c.msgs)
	}
	c.file, _ = os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	return c
}

func compact(msgs []*Message) []*Message {
	out := msgs[:0]
	for _, m := range msgs {
		if m.Kind != "deleted" {
			out = append(out, m)
		}
	}
	if len(out) > historyKeep {
		out = out[len(out)-historyKeep:]
	}
	return out
}

func (c *conversation) write(r logRecord) {
	if c.file == nil {
		return
	}
	data, _ := json.Marshal(r)
	_, _ = c.file.Write(append(data, '\n'))
}

// Add speichert eine neue Nachricht und vergibt ID und Zeitstempel.
func (h *History) Add(key string, m *Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m.ID = h.nextID()
	m.TS = time.Now().UnixMilli()
	c := h.get(key)
	c.msgs = append(c.msgs, m)
	if len(c.msgs) > historyKeep {
		c.msgs = c.msgs[len(c.msgs)-historyKeep:]
	}
	c.write(logRecord{Op: "msg", Msg: m})
}

// Find liefert eine Nachricht anhand der ID (nur aus dem Speicher).
func (h *History) Find(key, id string) *Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.get(key).msgs {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func (h *History) Edit(key, id, text string) *Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.get(key)
	for _, m := range c.msgs {
		if m.ID == id {
			m.Text = text
			m.Edited = time.Now().UnixMilli()
			c.write(logRecord{Op: "edit", ID: id, Txt: text, TS: m.Edited})
			cp := *m
			return &cp
		}
	}
	return nil
}

func (h *History) Delete(key, id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.get(key)
	for i, m := range c.msgs {
		if m.ID == id {
			c.msgs = append(c.msgs[:i], c.msgs[i+1:]...)
			c.write(logRecord{Op: "delete", ID: id})
			return true
		}
	}
	return false
}

// Page liefert bis zu limit Nachrichten vor der ID before (leer = neueste).
func (h *History) Page(key, before string, limit int) ([]Message, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	msgs := h.get(key).msgs
	end := len(msgs)
	if before != "" {
		for i, m := range msgs {
			if m.ID == before {
				end = i
				break
			}
		}
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	out := make([]Message, 0, end-start)
	for _, m := range msgs[start:end] {
		out = append(out, *m)
	}
	return out, start > 0
}

// LastTS liefert den Zeitstempel der letzten Nachricht (für DM-Sortierung).
func (h *History) LastTS(key string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	msgs := h.get(key).msgs
	if len(msgs) == 0 {
		return 0
	}
	return msgs[len(msgs)-1].TS
}
