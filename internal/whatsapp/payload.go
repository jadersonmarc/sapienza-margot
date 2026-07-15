package whatsapp

import "strings"

// evolutionWebhook is the native Evolution API webhook envelope (copied from
// rag-agente-go/internal/whatsapp/payload.go — the wire format is unchanged).
type evolutionWebhook struct {
	Event    string        `json:"event"`
	Instance string        `json:"instance"`
	Data     evolutionData `json:"data"`
}

type evolutionData struct {
	Key      evolutionKey     `json:"key"`
	PushName string           `json:"pushName"`
	Message  evolutionMessage `json:"message"`
}

type evolutionKey struct {
	RemoteJid string `json:"remoteJid"`
	FromMe    bool   `json:"fromMe"`
	ID        string `json:"id"`
}

type evolutionMessage struct {
	Conversation        string `json:"conversation"`
	ExtendedTextMessage *struct {
		Text string `json:"text"`
	} `json:"extendedTextMessage"`
}

// Inbound is the normalized, actionable view of a webhook event (exported so the
// pipeline package can consume it).
type Inbound struct {
	Instance   string
	Phone      string
	PushName   string
	Text       string
	ProviderID string
	FromMe     bool
}

// parseInbound normalizes a webhook into an Inbound. The bool is false when the
// event isn't an individual-chat message we can act on (group, broadcast, etc.).
func parseInbound(w evolutionWebhook) (Inbound, bool) {
	phone, ok := phoneFromJid(w.Data.Key.RemoteJid)
	if !ok {
		return Inbound{}, false
	}
	return Inbound{
		Instance:   w.Instance,
		Phone:      phone,
		PushName:   w.Data.PushName,
		Text:       messageText(w.Data.Message),
		ProviderID: w.Data.Key.ID,
		FromMe:     w.Data.Key.FromMe,
	}, true
}

// messageText extracts plain text from the supported message shapes. Non-text
// messages (media, audio, etc.) yield an empty string and are ignored in v1.
func messageText(m evolutionMessage) string {
	if m.Conversation != "" {
		return m.Conversation
	}
	if m.ExtendedTextMessage != nil {
		return m.ExtendedTextMessage.Text
	}
	return ""
}

// phoneFromJid turns "5511999999999@s.whatsapp.net" into "5511999999999".
// Group ("@g.us") and broadcast JIDs are rejected.
func phoneFromJid(jid string) (string, bool) {
	jid = strings.TrimSpace(jid)
	if jid == "" {
		return "", false
	}
	user, domain, found := strings.Cut(jid, "@")
	if !found {
		return jid, true
	}
	if domain != "s.whatsapp.net" {
		return "", false
	}
	return user, true
}
