package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

type Theme struct {
	Accent     string `json:"accent"`
	Background string `json:"background"`
}

type Cosmetics struct {
	NameColor    string   `json:"nameColor,omitempty"`
	NameGradient []string `json:"nameGradient,omitempty"`
	AvatarURL    string   `json:"avatarUrl,omitempty"`
	BannerURL    string   `json:"bannerUrl,omitempty"`
	BannerColor  string   `json:"bannerColor,omitempty"`
	Theme        Theme    `json:"theme"`
}

// Profile ist die öffentliche Sicht auf einen Account (vom Sessionserver).
type Profile struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"displayName"`
	Bio         string    `json:"bio,omitempty"`
	Cosmetics   Cosmetics `json:"cosmetics"`
	CreatedAt   int64     `json:"createdAt"`
}

var (
	usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_\-]{3,24}$`)
	channelRe  = regexp.MustCompile(`^#[a-zA-Z0-9_\-äöüÄÖÜß]{1,32}$`)
)

// jsonFile speichert eine Struktur atomar als JSON-Datei.
type jsonFile struct {
	mu   sync.Mutex
	path string
}

func newJSONFile(path string) *jsonFile { return &jsonFile{path: path} }

func (f *jsonFile) Load(v any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := os.ReadFile(f.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func (f *jsonFile) Save(v any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}
