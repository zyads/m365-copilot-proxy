package main

import (
	"os"
	"path/filepath"
	"time"
)

// Config — all from environment, with sane defaults where one exists.
type Config struct {
	Listen       string // address for the local server
	TenantID     string // Entra ID tenant (GUID or "organizations")
	ClientID     string // App registration (public client for device code)
	ClientSecret string // optional; confidential clients need it on every token call
	AuthMode     string // "device_code" (default) | "client_credentials"
	Scopes       string // space-separated delegated scopes
	TokenCache   string // path of the refresh-token cache file

	GraphBase      string // https://graph.microsoft.com/beta
	ConvPath       string // /copilot/conversations
	ChatPathFmt    string // /copilot/conversations/%s/chat
	ExtraBody      string // JSON merged into every chat body (future "model" knob etc.)
	ModelName      string // what we advertise on /v1/models and echo back
	TimeZone       string // locationHint.timeZone sent to Copilot
	RequestTimeout time.Duration
	ConvTTL        time.Duration // how long a Graph conversation is reused
	MaxRetries     int
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Listen:       env("LISTEN", "127.0.0.1:8080"),
		TenantID:     env("M365_TENANT_ID", "organizations"),
		ClientID:     os.Getenv("M365_CLIENT_ID"),
		ClientSecret: os.Getenv("M365_CLIENT_SECRET"),
		AuthMode:     env("AUTH_MODE", "device_code"),
		// .default = everything already consented on the app; nothing new to
		// request. offline_access is what gives us a refresh token.
		Scopes:         env("M365_SCOPES", "https://graph.microsoft.com/.default offline_access"),
		TokenCache:     env("TOKEN_CACHE", filepath.Join(home, ".config", "m365-copilot-proxy", "token.json")),
		GraphBase:      env("GRAPH_BASE", "https://graph.microsoft.com/beta"),
		ConvPath:       env("GRAPH_CONV_PATH", "/copilot/conversations"),
		ChatPathFmt:    env("GRAPH_CHAT_PATH_FMT", "/copilot/conversations/%s/chat"),
		ExtraBody:      env("GRAPH_EXTRA_BODY", ""),
		ModelName:      env("MODEL_NAME", "m365-copilot"),
		TimeZone:       env("M365_TIMEZONE", "UTC"),
		RequestTimeout: 300 * time.Second,
		ConvTTL:        2 * time.Hour,
		MaxRetries:     4,
	}
}
