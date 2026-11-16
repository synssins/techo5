package config

import (
	"fmt"
	"net/url"
	"strings"
)

// AgentHub is listening without a wake word (feature/agenthub): every stretch of speech the device hears
// goes to an AgentHub server, which transcribes it, keeps only what was said to one of its agents, and
// sends the agent's spoken answer back. Off unless turned on, and the device's own: a restore never
// turns it on or carries another device's key (KeepDevice, ClearSecrets).
type AgentHub struct {
	On bool `json:"on,omitempty"`
	// URL is the server's base, like http://192.168.1.86:8787.
	URL string `json:"url,omitempty"`
	// Key is this device's key from the server (Settings > Voice > Devices), sent as a bearer token. It
	// is a secret: never shown again once saved.
	Key string `json:"key,omitempty"`
}

// Ready is whether listening can run: turned on, with an address and a key.
func (a AgentHub) Ready() bool { return a.On && a.URL != "" && a.Key != "" }

// ValidAgentHubURL cleans up an address typed for AgentHub.URL: empty, or http(s) with a host.
func ValidAgentHubURL(s string) (string, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("should be an address like http://192.168.1.86:8787")
	}
	return s, nil
}

type AgentHubWriter struct{ st *Store }

func (w AgentHubWriter) On(on bool) error {
	return w.st.Update(func(c *Config) { c.AgentHub.On = on })
}

func (w AgentHubWriter) URL(s string) error {
	s, err := ValidAgentHubURL(s)
	if err != nil {
		return err
	}
	return w.st.Update(func(c *Config) { c.AgentHub.URL = s })
}

func (w AgentHubWriter) Key(key string) error {
	key = strings.TrimSpace(key)
	if strings.ContainsAny(key, "\r\n") {
		return fmt.Errorf("a key is one line")
	}
	return w.st.Update(func(c *Config) { c.AgentHub.Key = key })
}
