package github

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Format prints a Token without its value, with any verb, so a token passed
// to a log line or an error text by mistake does not leak. Read Value to use
// the token.
func (t Token) Format(f fmt.State, _ rune) {
	fmt.Fprintf(f, "github.Token{InstallationID: %d, ExpiresAt: %s, Value: redacted}",
		t.InstallationID, t.ExpiresAt.Format(time.RFC3339))
}

// LogValue logs a Token without its value.
func (t Token) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int64("installation_id", t.InstallationID),
		slog.Time("expires_at", t.ExpiresAt),
	)
}

// MarshalJSON encodes a Token without its value, so a token never reaches a
// caller inside a JSON answer.
func (t Token) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		InstallationID int64     `json:"installation_id"`
		ExpiresAt      time.Time `json:"expires_at"`
	}{t.InstallationID, t.ExpiresAt})
}
