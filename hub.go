package main

import (
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxMessageLen = 2000
	maxTopicLen   = 300
	maxChannels   = 500
)

// ChannelState ist der dauerhaft gespeicherte Zustand eines Channels.
// Mitgliedschaften sind (wie bei Discord) dauerhaft – wer einmal beitritt,
// bleibt Mitglied, bis er den Channel verlässt oder gekickt wird.
type ChannelState struct {
	Name      string          `json:"name"`
	Topic     string          `json:"topic"`
	Owner     string          `json:"owner"`
	Ops       map[string]bool `json:"ops"`
	Bans      map[string]bool `json:"bans"`
	Members   map[string]bool `json:"members"`
	CreatedAt int64           `json:"createdAt"`
}

type hubState struct {
	Channels   map[string]*ChannelState   `json:"channels"`   // key: lowercase Name
	Profiles   map[string]Profile         `json:"profiles"`   // zuletzt bekannte Profile
	DMPartners map[string]map[string]bool `json:"dmPartners"` // user -> partner
}

type Hub struct {
	mu    sync.Mutex
	cfg   config
	st    hubState
	file  *jsonFile
	hist  *History
	conns map[string]map[*Client]bool // user -> offene Verbindungen
}

// ---------- Ausgehende Strukturen ----------

type MemberInfo struct {
	Profile
	Online bool   `json:"online"`
	Role   string `json:"role"` // "owner" | "op" | "member"
}

type ChannelInfo struct {
	Name    string       `json:"name"`
	Topic   string       `json:"topic"`
	Owner   string       `json:"owner"`
	Members []MemberInfo `json:"members"`
}

type DMInfo struct {
	User   Profile `json:"user"`
	Online bool    `json:"online"`
	LastTS int64   `json:"lastTs"`
}

// ---------- Aufbau ----------

func newHub(cfg config) (*Hub, error) {
	hist, err := newHistory(filepath.Join(cfg.DataDir, "history"))
	if err != nil {
		return nil, err
	}
	h := &Hub{
		cfg:   cfg,
		file:  newJSONFile(filepath.Join(cfg.DataDir, "state.json")),
		hist:  hist,
		conns: map[string]map[*Client]bool{},
	}
	if err := h.file.Load(&h.st); err != nil {
		return nil, err
	}
	if h.st.Channels == nil {
		h.st.Channels = map[string]*ChannelState{}
	}
	if h.st.Profiles == nil {
		h.st.Profiles = map[string]Profile{}
	}
	if h.st.DMPartners == nil {
		h.st.DMPartners = map[string]map[string]bool{}
	}
	for _, name := range cfg.DefaultChannels {
		if h.st.Channels[strings.ToLower(name)] == nil {
			h.st.Channels[strings.ToLower(name)] = newChannel(name, "")
		}
	}
	h.save()
	return h, nil
}

func newChannel(name, owner string) *ChannelState {
	ch := &ChannelState{
		Name: name, Owner: owner, CreatedAt: time.Now().Unix(),
		Ops: map[string]bool{}, Bans: map[string]bool{}, Members: map[string]bool{},
	}
	if owner != "" {
		ch.Members[owner] = true
	}
	return ch
}

func (h *Hub) save() {
	if err := h.file.Save(&h.st); err != nil {
		log.Printf("Speichern fehlgeschlagen: %v", err)
	}
}

func (h *Hub) onlineCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// ---------- Hilfsfunktionen (Aufrufer hält h.mu) ----------

func (h *Hub) isOnline(user string) bool { return len(h.conns[user]) > 0 }

func (h *Hub) role(ch *ChannelState, user string) string {
	switch {
	case ch.Owner == user:
		return "owner"
	case ch.Ops[user] || h.cfg.Admins[user]:
		return "op"
	}
	return "member"
}

func (h *Hub) isOp(ch *ChannelState, user string) bool { return h.role(ch, user) != "member" }

func (h *Hub) toUser(user string, v any) {
	for c := range h.conns[user] {
		c.emit(v)
	}
}

func (h *Hub) toChannel(ch *ChannelState, v any) {
	for m := range ch.Members {
		h.toUser(m, v)
	}
}

func (h *Hub) toAll(v any) {
	for user := range h.conns {
		h.toUser(user, v)
	}
}

func (h *Hub) profile(user string) Profile {
	if p, ok := h.st.Profiles[user]; ok {
		return p
	}
	return Profile{Username: user, DisplayName: user}
}

func (h *Hub) channelInfo(ch *ChannelState) ChannelInfo {
	info := ChannelInfo{Name: ch.Name, Topic: ch.Topic, Owner: ch.Owner}
	for m := range ch.Members {
		info.Members = append(info.Members, MemberInfo{Profile: h.profile(m), Online: h.isOnline(m), Role: h.role(ch, m)})
	}
	sort.Slice(info.Members, func(i, j int) bool { return info.Members[i].Username < info.Members[j].Username })
	return info
}

func dmKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return "dm:" + a + ":" + b
}

func (h *Hub) channel(name string) *ChannelState { return h.st.Channels[strings.ToLower(name)] }

// ---------- Verbindungen ----------

func (h *Hub) connect(c *Client, p *Profile) {
	user := strings.ToLower(p.Username)

	h.mu.Lock()
	defer h.mu.Unlock()

	c.user, c.profile = user, p
	c.authed.Store(true)
	_, known := h.st.Profiles[user]
	h.st.Profiles[user] = *p
	var autoJoined []*ChannelState
	if !known {
		// Neue Nutzer automatisch in die Standard-Channels setzen.
		for _, name := range h.cfg.DefaultChannels {
			if ch := h.channel(name); ch != nil && !ch.Bans[user] && !ch.Members[user] {
				ch.Members[user] = true
				autoJoined = append(autoJoined, ch)
			}
		}
	}
	h.save()

	wasOnline := h.isOnline(user)
	if h.conns[user] == nil {
		h.conns[user] = map[*Client]bool{}
	}
	h.conns[user][c] = true

	var channels []ChannelInfo
	for _, ch := range h.st.Channels {
		if ch.Members[user] {
			channels = append(channels, h.channelInfo(ch))
		}
	}
	sort.Slice(channels, func(i, j int) bool { return channels[i].Name < channels[j].Name })

	var dms []DMInfo
	for partner := range h.st.DMPartners[user] {
		dms = append(dms, DMInfo{User: h.profile(partner), Online: h.isOnline(partner), LastTS: h.hist.LastTS(dmKey(user, partner))})
	}
	sort.Slice(dms, func(i, j int) bool { return dms[i].LastTS > dms[j].LastTS })

	c.emit(map[string]any{
		"op":       "welcome",
		"me":       p,
		"isAdmin":  h.cfg.Admins[user],
		"server":   map[string]string{"name": h.cfg.Name, "motd": h.cfg.MOTD},
		"channels": channels,
		"dms":      dms,
	})

	for _, ch := range autoJoined {
		h.toChannel(ch, map[string]any{
			"op": "member_join", "channel": ch.Name,
			"member": MemberInfo{Profile: *p, Online: true, Role: h.role(ch, user)},
		})
	}
	if !wasOnline {
		h.toAll(map[string]any{"op": "presence", "username": user, "online": true})
	}
	// Profil kann sich seit dem letzten Besuch geändert haben.
	if known {
		h.toAll(map[string]any{"op": "user_update", "user": p})
	}
	log.Printf("%s verbunden (%s)", p.Username, c.conn.RemoteAddr())
}

func (h *Hub) disconnect(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	close(c.send)
	if c.user == "" {
		return
	}
	delete(h.conns[c.user], c)
	if len(h.conns[c.user]) == 0 {
		delete(h.conns, c.user)
		h.toAll(map[string]any{"op": "presence", "username": c.user, "online": false})
	}
}

// ---------- Paket-Verarbeitung ----------

func (h *Hub) handle(c *Client, f Frame) {
	switch f.Op {
	case "ping":
		c.emit(map[string]any{"op": "pong", "ref": f.Ref})
	case "join":
		h.join(c, f)
	case "part":
		h.part(c, f)
	case "msg":
		h.message(c, f)
	case "edit":
		h.edit(c, f)
	case "delete":
		h.del(c, f)
	case "history":
		h.history(c, f)
	case "typing":
		h.typing(c, f)
	case "topic":
		h.topic(c, f)
	case "list":
		h.list(c, f)
	case "whois":
		h.whois(c, f)
	case "kick", "ban":
		h.kick(c, f, f.Op == "ban")
	case "unban":
		h.unban(c, f)
	case "op", "deop":
		h.setOp(c, f, f.Op == "op")
	case "delete_channel":
		h.deleteChannel(c, f)
	case "open_dm":
		h.openDM(c, f)
	case "refresh_profile":
		h.refreshProfile(c, f)
	default:
		c.fail(f.Ref, "unknown_op", "Unbekannte Operation: "+f.Op)
	}
}

func (h *Hub) join(c *Client, f Frame) {
	if !channelRe.MatchString(f.Channel) {
		c.fail(f.Ref, "bad_channel", "Channelnamen beginnen mit # (max. 32 Zeichen, keine Leerzeichen)")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.channel(f.Channel)
	if ch == nil {
		if len(h.st.Channels) >= maxChannels {
			c.fail(f.Ref, "limit", "Maximale Anzahl an Channels erreicht")
			return
		}
		ch = newChannel(f.Channel, c.user)
		h.st.Channels[strings.ToLower(f.Channel)] = ch
	} else {
		if ch.Bans[c.user] {
			c.fail(f.Ref, "banned", "Du bist aus "+ch.Name+" gebannt")
			return
		}
		if ch.Members[c.user] {
			c.emit(map[string]any{"op": "channel", "ref": f.Ref, "channel": h.channelInfo(ch)})
			return
		}
		ch.Members[c.user] = true
	}
	h.save()
	h.toUser(c.user, map[string]any{"op": "channel", "ref": f.Ref, "channel": h.channelInfo(ch)})
	h.toChannel(ch, map[string]any{
		"op": "member_join", "channel": ch.Name,
		"member": MemberInfo{Profile: h.profile(c.user), Online: true, Role: h.role(ch, c.user)},
	})
}

func (h *Hub) part(c *Client, f Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.channel(f.Channel)
	if ch == nil || !ch.Members[c.user] {
		c.fail(f.Ref, "not_member", "Du bist nicht in diesem Channel")
		return
	}
	h.toChannel(ch, map[string]any{"op": "member_part", "channel": ch.Name, "username": c.user, "reason": f.Reason})
	delete(ch.Members, c.user)
	h.save()
}

// resolveTarget prüft das Ziel einer Nachricht und liefert den Verlaufsschlüssel.
// Aufrufer hält h.mu.
func (h *Hub) resolveTarget(c *Client, target string) (key string, ch *ChannelState, dmUser string, ok bool) {
	if strings.HasPrefix(target, "#") {
		ch = h.channel(target)
		if ch == nil || !ch.Members[c.user] {
			return "", nil, "", false
		}
		return "ch:" + strings.ToLower(ch.Name), ch, "", true
	}
	u := strings.ToLower(target)
	if _, known := h.st.Profiles[u]; !known {
		return "", nil, "", false
	}
	return dmKey(c.user, u), nil, u, true
}

func cleanText(s string) (string, bool) {
	s = strings.TrimRight(s, " \t\r\n")
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxMessageLen {
		return "", false
	}
	return s, true
}

func (h *Hub) message(c *Client, f Frame) {
	text, ok := cleanText(f.Text)
	if !ok {
		c.fail(f.Ref, "bad_message", "Nachricht leer oder länger als 2000 Zeichen")
		return
	}
	if !c.floodCheck() {
		c.fail(f.Ref, "flood", "Langsamer bitte – zu viele Nachrichten")
		return
	}
	kind := "text"
	if f.Kind == "action" {
		kind = "action"
	}

	// DM an einen Nutzer, der auf diesem Server noch nie war: Profil beim
	// Sessionserver nachschlagen (außerhalb des Locks).
	if !strings.HasPrefix(f.Target, "#") {
		h.ensureProfile(strings.ToLower(f.Target))
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	key, ch, dmUser, ok := h.resolveTarget(c, f.Target)
	if !ok {
		c.fail(f.Ref, "bad_target", "Unbekannter Channel/Nutzer oder kein Mitglied")
		return
	}
	m := &Message{From: c.user, Text: text, Kind: kind}
	if ch != nil {
		m.Target = ch.Name
	} else {
		m.Target = dmUser
	}
	h.hist.Add(key, m)
	out := map[string]any{"op": "message", "ref": f.Ref, "message": m}
	if ch != nil {
		h.toChannel(ch, out)
		return
	}
	// DM: beide Seiten merken sich die Unterhaltung.
	newThread := !h.st.DMPartners[dmUser][c.user]
	for _, pair := range [][2]string{{c.user, dmUser}, {dmUser, c.user}} {
		if h.st.DMPartners[pair[0]] == nil {
			h.st.DMPartners[pair[0]] = map[string]bool{}
		}
		h.st.DMPartners[pair[0]][pair[1]] = true
	}
	if newThread {
		h.save()
		h.toUser(dmUser, map[string]any{"op": "dm_open", "dm": DMInfo{User: h.profile(c.user), Online: true, LastTS: m.TS}})
	}
	h.toUser(c.user, out)
	if dmUser != c.user {
		h.toUser(dmUser, out)
	}
}

func (h *Hub) ensureProfile(user string) {
	if !usernameRe.MatchString(user) {
		return
	}
	h.mu.Lock()
	_, known := h.st.Profiles[user]
	h.mu.Unlock()
	if known {
		return
	}
	p, err := fetchProfile(h.cfg.SessionURL, user)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.st.Profiles[user] = *p
	h.save()
	h.mu.Unlock()
}

func (h *Hub) openDM(c *Client, f Frame) {
	user := strings.ToLower(f.Username)
	h.ensureProfile(user)
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.st.Profiles[user]; !ok {
		c.fail(f.Ref, "unknown_user", "Diesen Nutzer gibt es nicht")
		return
	}
	c.emit(map[string]any{"op": "dm_open", "ref": f.Ref, "dm": DMInfo{
		User: h.profile(user), Online: h.isOnline(user), LastTS: h.hist.LastTS(dmKey(c.user, user)),
	}})
}

// broadcastConv schickt ein Paket an alle Beteiligten einer Unterhaltung.
func (h *Hub) broadcastConv(c *Client, ch *ChannelState, dmUser string, v any) {
	if ch != nil {
		h.toChannel(ch, v)
		return
	}
	h.toUser(c.user, v)
	if dmUser != c.user {
		h.toUser(dmUser, v)
	}
}

func (h *Hub) edit(c *Client, f Frame) {
	text, ok := cleanText(f.Text)
	if !ok {
		c.fail(f.Ref, "bad_message", "Nachricht leer oder zu lang")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key, ch, dmUser, ok := h.resolveTarget(c, f.Target)
	if !ok {
		c.fail(f.Ref, "bad_target", "Unbekanntes Ziel")
		return
	}
	m := h.hist.Find(key, f.Edit)
	if m == nil || m.From != c.user {
		c.fail(f.Ref, "forbidden", "Du kannst nur eigene Nachrichten bearbeiten")
		return
	}
	updated := h.hist.Edit(key, f.Edit, text)
	h.broadcastConv(c, ch, dmUser, map[string]any{"op": "message_edit", "message": updated})
}

func (h *Hub) del(c *Client, f Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key, ch, dmUser, ok := h.resolveTarget(c, f.Target)
	if !ok {
		c.fail(f.Ref, "bad_target", "Unbekanntes Ziel")
		return
	}
	m := h.hist.Find(key, f.Edit)
	if m == nil || (m.From != c.user && (ch == nil || !h.isOp(ch, c.user))) {
		c.fail(f.Ref, "forbidden", "Keine Berechtigung")
		return
	}
	target := m.Target
	h.hist.Delete(key, f.Edit)
	h.broadcastConv(c, ch, dmUser, map[string]any{"op": "message_delete", "target": target, "from": m.From, "id": f.Edit})
}

func (h *Hub) history(c *Client, f Frame) {
	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	h.mu.Lock()
	key, _, _, ok := h.resolveTarget(c, f.Target)
	h.mu.Unlock()
	if !ok {
		c.fail(f.Ref, "bad_target", "Unbekanntes Ziel")
		return
	}
	msgs, more := h.hist.Page(key, f.Before, limit)
	c.emit(map[string]any{"op": "history", "ref": f.Ref, "target": f.Target, "messages": msgs, "hasMore": more})
}

func (h *Hub) typing(c *Client, f Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ch, dmUser, ok := h.resolveTarget(c, f.Target)
	if !ok {
		return
	}
	if ch != nil {
		for m := range ch.Members {
			if m != c.user {
				h.toUser(m, map[string]any{"op": "typing", "target": ch.Name, "username": c.user})
			}
		}
	} else if dmUser != c.user {
		h.toUser(dmUser, map[string]any{"op": "typing", "target": c.user, "username": c.user})
	}
}

func (h *Hub) topic(c *Client, f Frame) {
	if utf8.RuneCountInString(f.Topic) > maxTopicLen {
		c.fail(f.Ref, "bad_topic", "Topic ist zu lang")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.channel(f.Channel)
	if ch == nil || !h.isOp(ch, c.user) {
		c.fail(f.Ref, "forbidden", "Nur Channel-Operatoren dürfen das Topic ändern")
		return
	}
	ch.Topic = strings.TrimSpace(f.Topic)
	h.save()
	h.toChannel(ch, map[string]any{"op": "topic", "channel": ch.Name, "topic": ch.Topic, "by": c.user})
}

func (h *Hub) list(c *Client, f Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	type entry struct {
		Name    string `json:"name"`
		Topic   string `json:"topic"`
		Members int    `json:"members"`
		Joined  bool   `json:"joined"`
	}
	var out []entry
	for _, ch := range h.st.Channels {
		if ch.Bans[c.user] {
			continue
		}
		out = append(out, entry{ch.Name, ch.Topic, len(ch.Members), ch.Members[c.user]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Members > out[j].Members })
	c.emit(map[string]any{"op": "channel_list", "ref": f.Ref, "channels": out})
}

func (h *Hub) whois(c *Client, f Frame) {
	user := strings.ToLower(f.Username)
	h.ensureProfile(user)
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.st.Profiles[user]
	if !ok {
		c.fail(f.Ref, "unknown_user", "Diesen Nutzer gibt es nicht")
		return
	}
	var chans []string
	for _, ch := range h.st.Channels {
		if ch.Members[user] && ch.Members[c.user] {
			chans = append(chans, ch.Name)
		}
	}
	sort.Strings(chans)
	c.emit(map[string]any{"op": "whois", "ref": f.Ref, "user": p, "online": h.isOnline(user), "sharedChannels": chans})
}

func (h *Hub) kick(c *Client, f Frame, ban bool) {
	target := strings.ToLower(f.Username)
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.channel(f.Channel)
	if ch == nil || !h.isOp(ch, c.user) {
		c.fail(f.Ref, "forbidden", "Nur Channel-Operatoren dürfen das")
		return
	}
	if target == ch.Owner || (h.cfg.Admins[target] && !h.cfg.Admins[c.user]) || (ch.Ops[target] && h.role(ch, c.user) != "owner" && !h.cfg.Admins[c.user]) {
		c.fail(f.Ref, "forbidden", "Diesen Nutzer kannst du nicht entfernen")
		return
	}
	if ban {
		ch.Bans[target] = true
		delete(ch.Ops, target)
	}
	if ch.Members[target] {
		h.toChannel(ch, map[string]any{"op": "member_part", "channel": ch.Name, "username": target, "kickedBy": c.user, "banned": ban, "reason": f.Reason})
		delete(ch.Members, target)
	}
	h.save()
}

func (h *Hub) unban(c *Client, f Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.channel(f.Channel)
	if ch == nil || !h.isOp(ch, c.user) {
		c.fail(f.Ref, "forbidden", "Nur Channel-Operatoren dürfen das")
		return
	}
	delete(ch.Bans, strings.ToLower(f.Username))
	h.save()
	c.emit(map[string]any{"op": "ok", "ref": f.Ref})
}

func (h *Hub) setOp(c *Client, f Frame, grant bool) {
	target := strings.ToLower(f.Username)
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.channel(f.Channel)
	if ch == nil || (ch.Owner != c.user && !h.cfg.Admins[c.user]) {
		c.fail(f.Ref, "forbidden", "Nur der Channel-Besitzer darf Operatoren ernennen")
		return
	}
	if !ch.Members[target] {
		c.fail(f.Ref, "not_member", "Nutzer ist nicht im Channel")
		return
	}
	if grant {
		ch.Ops[target] = true
	} else {
		delete(ch.Ops, target)
	}
	h.save()
	h.toChannel(ch, map[string]any{"op": "role", "channel": ch.Name, "username": target, "role": h.role(ch, target)})
}

func (h *Hub) deleteChannel(c *Client, f Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.channel(f.Channel)
	if ch == nil || (ch.Owner != c.user && !h.cfg.Admins[c.user]) {
		c.fail(f.Ref, "forbidden", "Nur der Besitzer oder ein Admin darf den Channel löschen")
		return
	}
	h.toChannel(ch, map[string]any{"op": "channel_deleted", "channel": ch.Name})
	delete(h.st.Channels, strings.ToLower(ch.Name))
	h.save()
}

func (h *Hub) refreshProfile(c *Client, f Frame) {
	p, err := fetchProfile(h.cfg.SessionURL, c.user)
	if err != nil {
		c.fail(f.Ref, "session_error", "Profil konnte nicht geladen werden")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.st.Profiles[c.user] = *p
	h.save()
	h.toAll(map[string]any{"op": "user_update", "user": p})
}
