package main

// events.go — token-only domain events (D3: "all events and order snapshots
// carry usr_/adr_ tokens only"). The payload struct has NO PII field — it is
// physically impossible for this code path to emit a name/phone/email/address.
// The PII scanner (tools/piiscan) checks the emitted golden traffic to prove it.
import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/shop-platform/shop/libs/eventbus"
)

const profileSchemaVersion = 1

// profileEventPayload is the ENTIRE payload of every profile event. Tokens and
// classifications only — never a PII value.
type profileEventPayload struct {
	UserToken    string   `json:"user_token"`
	Jurisdiction string   `json:"jurisdiction"`
	Action       string   `json:"action"`      // created | updated | address_added | erased
	AddrTokens   []string `json:"addr_tokens"` // adr_ tokens touched
}

// eventBuilder mints envelopes. traceID is per-request when known, else random.
type eventBuilder struct {
	region string
}

func newEventBuilder(region string) *eventBuilder { return &eventBuilder{region: region} }

// profileChanged builds a token-only envelope for the given topic/event_type. The
// event_type MUST equal the topic-schema const (profile.updated / profile.erased);
// the finer-grained verb lives in payload.action.
func (b *eventBuilder) profileChanged(eventType, action, userToken, jurisdiction string, addrTokens []string) eventbus.Envelope {
	if addrTokens == nil {
		addrTokens = []string{}
	}
	payload := profileEventPayload{
		UserToken: userToken, Jurisdiction: jurisdiction, Action: action, AddrTokens: addrTokens,
	}
	env, _ := eventbus.NewEnvelope(
		newToken("evt"),
		eventType,
		randTraceID(),
		eventbus.Aggregate{Type: "profile", ID: userToken, Region: b.region},
		profileSchemaVersion,
		payload,
		time.Now().UTC(),
	)
	return env
}

func randTraceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
