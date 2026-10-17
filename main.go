// mirc-chatserver – selbst gehosteter MIRC-Chatserver.
//
// Transport: TCP (optional TLS), pro Zeile ein JSON-Objekt (siehe PROTOCOL.md).
// Anmeldung: Der Client schickt einen Einmal-Token, den er sich beim
// Sessionserver für genau diesen Server geholt hat. Der Chatserver fragt beim
// Sessionserver nach, ob der Token gültig ist, und lässt den Nutzer erst dann
// herein.
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type config struct {
	Name            string
	MOTD            string
	SessionURL      string   // intern, für die Token-Prüfung
	PublicSession   string   // wird den Clients angekündigt
	Addresses       []string // Adressen, unter denen Clients diesen Server erreichen
	DefaultChannels []string
	Admins          map[string]bool
	DataDir         string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return port
}

func main() {
	listen := flag.String("listen", env("MIRC_LISTEN", ":6680"), "Klartext-Port (leer = aus)")
	tlsListen := flag.String("tls-listen", env("MIRC_TLS_LISTEN", ":6697"), "TLS-Port (leer = aus)")
	tlsCert := flag.String("tls-cert", env("MIRC_TLS_CERT", ""), "TLS-Zertifikat (leer = selbstsigniert erzeugen)")
	tlsKey := flag.String("tls-key", env("MIRC_TLS_KEY", ""), "TLS-Schlüssel")
	name := flag.String("name", env("MIRC_NAME", "Mein MIRC-Server"), "Servername")
	motd := flag.String("motd", env("MIRC_MOTD", "Willkommen auf diesem MIRC-Server!"), "Message of the Day")
	session := flag.String("session", env("MIRC_SESSION_URL", "http://localhost:8440"), "URL des Sessionservers")
	publicSession := flag.String("public-session", env("MIRC_PUBLIC_SESSION_URL", ""), "Sessionserver-URL, die Clients angezeigt wird (Standard: wie -session)")
	addresses := flag.String("addresses", env("MIRC_ADDRESSES", ""), "Öffentliche Adressen host:port, unter denen Clients verbinden (kommagetrennt)")
	channels := flag.String("channels", env("MIRC_CHANNELS", "#allgemein,#offtopic"), "Standard-Channels")
	admins := flag.String("admins", env("MIRC_ADMINS", ""), "Server-Admins (Benutzernamen)")
	dataDir := flag.String("data", env("MIRC_DATA", "./data"), "Datenverzeichnis")
	flag.Parse()

	cfg := config{
		Name: *name, MOTD: *motd, SessionURL: strings.TrimRight(*session, "/"),
		Admins: map[string]bool{}, DataDir: *dataDir,
	}
	cfg.PublicSession = strings.TrimRight(*publicSession, "/")
	if cfg.PublicSession == "" {
		cfg.PublicSession = cfg.SessionURL
	}
	for _, c := range splitList(*channels) {
		if channelRe.MatchString(c) {
			cfg.DefaultChannels = append(cfg.DefaultChannels, c)
		}
	}
	for _, a := range splitList(*admins) {
		cfg.Admins[strings.ToLower(a)] = true
	}
	cfg.Addresses = splitList(strings.ToLower(*addresses))
	if len(cfg.Addresses) == 0 {
		for _, l := range []string{*listen, *tlsListen} {
			if p := portOf(l); p != "" {
				cfg.Addresses = append(cfg.Addresses, "localhost:"+p, "127.0.0.1:"+p)
			}
		}
		log.Printf("WARNUNG: -addresses/MIRC_ADDRESSES nicht gesetzt – nur lokale Verbindungen (%s) können sich anmelden.", strings.Join(cfg.Addresses, ", "))
	}

	hub, err := newHub(cfg)
	if err != nil {
		log.Fatal(err)
	}

	started := 0
	if *listen != "" {
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("Klartext auf %s", *listen)
		go acceptLoop(ln, hub, false)
		started++
	}
	if *tlsListen != "" {
		cert, err := loadOrCreateCert(*tlsCert, *tlsKey, *dataDir, cfg.Addresses)
		if err != nil {
			log.Fatalf("TLS: %v", err)
		}
		ln, err := tls.Listen("tcp", *tlsListen, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("TLS auf %s", *tlsListen)
		go acceptLoop(ln, hub, true)
		started++
	}
	if started == 0 {
		log.Fatal("Weder -listen noch -tls-listen gesetzt")
	}
	log.Printf("MIRC-Chatserver »%s« bereit · Sessionserver %s · akzeptierte Adressen: %s", cfg.Name, cfg.SessionURL, strings.Join(cfg.Addresses, ", "))
	select {}
}

func acceptLoop(ln net.Listener, hub *Hub, secure bool) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			log.Printf("accept: %v", err)
			time.Sleep(time.Second)
			continue
		}
		c := newClient(hub, conn, secure)
		go c.writePump()
		go c.readPump()
	}
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

type verifyError struct {
	msg       string
	issuedFor string
}

func (e *verifyError) Error() string { return e.msg }

// verifyToken lässt den Sessionserver prüfen, ob der Token gültig und für
// diesen Server ausgestellt ist, und liefert das Profil des Nutzers.
func verifyToken(cfg config, token string) (*Profile, error) {
	body, _ := json.Marshal(map[string]any{"token": token, "addresses": cfg.Addresses})
	resp, err := httpClient.Post(cfg.SessionURL+"/api/session/verify", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, &verifyError{msg: "Sessionserver nicht erreichbar"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e struct {
			Error     string `json:"error"`
			IssuedFor string `json:"issuedFor"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("Sessionserver antwortet mit %d", resp.StatusCode)
		}
		return nil, &verifyError{msg: e.Error, issuedFor: e.IssuedFor}
	}
	var p Profile
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

func fetchProfile(sessionURL, username string) (*Profile, error) {
	resp, err := httpClient.Get(sessionURL + "/api/profile/" + url.PathEscape(username))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var p Profile
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, err
	}
	return &p, nil
}
