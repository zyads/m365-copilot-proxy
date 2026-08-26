package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config — all from environment, with sane defaults where one exists.
type Config struct {
	Listen       string // address for the local server
	TenantID     string // Entra ID tenant (GUID or "organizations")
	ClientID     string // App registration (public client for device code)
	ClientSecret string // optional; confidential clients need it on every token call
	AuthMode     string // "device_code" (default) | "browser" (auth-code+PKCE, CAP-friendly) | "client_credentials"
	Scopes       string // space-separated delegated scopes
	TokenCache   string // path of the refresh-token cache file

	GraphBase      string // https://graph.microsoft.com/beta
	ConvPath       string // /copilot/conversations
	ChatPathFmt    string // /copilot/conversations/%s/chat
	ExtraBody      string // JSON merged into every chat body (future "model" knob etc.)
	ModelName      string // what we advertise on /v1/models and echo back
	Persona        string // "" = built-in coding-agent persona, "off" = none, else custom text
	RepoRoot       string // force the repo-map root; else parsed from the system prompt
	RepoMap        bool   // inject the repo map context (default on)
	Enforce        bool   // re-prompt once when Copilot refuses to use tools (default on)
	DebugDir       string // if set, dump every prompt/reply pair here as JSON
	DebugRaw       bool   // DEBUG_RAW=1 keeps dumps un-redacted (local use only)
	ToolResultMax  int    // bytes of one tool result kept verbatim (head+tail beyond)
	AutoUpdate     bool   // check origin/main on startup + every UpdateInterval
	UpdateInterval time.Duration
	RepoDir        string // git checkout to update from (default: dir of the binary)
	Thinking       bool   // ask for <thinking> and surface it as reasoning_content (default on)
	InstrInMessage bool   // put persona/protocol/catalog in the message text (default on); off = Graph contexts[]
	Sources        bool   // SOURCES=on keeps Copilot attributions/citations (default off: stripped)
	Prime          bool   // PRIME=off disables the role-commitment turn on fresh conversations
	TimeZone       string // locationHint.timeZone sent to Copilot
	RequestTimeout time.Duration
	ConvTTL        time.Duration // how long a Graph conversation is reused
	MaxRetries     int
}

// listenPort extracts the port from Listen ("127.0.0.1:8080" -> "8080").
func (c Config) listenPort() string {
	if i := strings.LastIndex(c.Listen, ":"); i >= 0 {
		return c.Listen[i+1:]
	}
	return "8080"
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
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
		Persona:        os.Getenv("AGENT_PERSONA"),
		RepoRoot:       os.Getenv("REPO_ROOT"),
		RepoMap:        env("REPO_MAP", "on") != "off",
		Enforce:        env("ENFORCE_TOOLS", "on") != "off",
		DebugDir:       os.Getenv("DEBUG_DIR"),
		DebugRaw:       os.Getenv("DEBUG_RAW") == "1",
		ToolResultMax:  envInt("MAX_TOOL_RESULT", defaultToolResultBudget),
		AutoUpdate:     env("AUTO_UPDATE", "on") != "off",
		UpdateInterval: time.Duration(envInt("UPDATE_INTERVAL_MIN", 360)) * time.Minute,
		RepoDir:        os.Getenv("REPO_DIR"),
		Thinking:       env("THINKING", "on") != "off",
		InstrInMessage: env("INSTRUCTIONS_IN", "message") != "contexts",
		Sources:        env("SOURCES", "off") == "on",
		Prime:          env("PRIME", "on") != "off",
		TimeZone:       env("M365_TIMEZONE", "UTC"),
		RequestTimeout: 300 * time.Second,
		ConvTTL:        2 * time.Hour,
		MaxRetries:     4,
	}
}
