// Package server implements the Passway tunnel server.
package server

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"

	"github.com/mohammed19n/passway/internal/protocol"
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)

var reserved = map[string]bool{
	"www": true, "api": true, "app": true, "edge": true,
	"_tunnel": true, "admin": true, "status": true, "docs": true,
}

// ValidateName reports whether name is an allowed subdomain. On failure it
// returns a *protocol.Error with CodeInvalidName.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return &protocol.Error{Code: protocol.CodeInvalidName,
			Message: "name must be 3-32 chars of a-z, 0-9 and '-', not starting or ending with '-'"}
	}
	if reserved[name] {
		return &protocol.Error{Code: protocol.CodeInvalidName, Message: fmt.Sprintf("name %q is reserved", name)}
	}
	if w := blockedWord(name); w != "" {
		return &protocol.Error{Code: protocol.CodeInvalidName, Message: fmt.Sprintf("name %q contains a blocked word", name)}
	}
	return nil
}

// blockedWords are substrings that make a subdomain unavailable (phishing /
// brand-impersonation abuse). Webhook senders (Stripe, GitHub, ...) are left
// out on purpose: "stripe-hooks" is exactly the name users pick.
var blockedWords = []string{"paypal", "login", "signin", "logon", "bank", "secure", "verify", "wallet", "password", "apple", "google", "microsoft", "office365", "outlook", "amazon", "facebook", "instagram", "whatsapp", "netflix", "coinbase", "binance", "metamask"}

// blockedWord returns the first blocked word contained in name, or "".
func blockedWord(name string) string {
	for _, w := range blockedWords {
		if strings.Contains(name, w) {
			return w
		}
	}
	return ""
}

var adjectives = []string{
	"quick", "brave", "calm", "clever", "cosmic", "crisp", "daring", "eager", "fancy", "fuzzy",
	"gentle", "giant", "glad", "golden", "happy", "humble", "jolly", "keen", "kind", "lively",
	"lucky", "mellow", "merry", "mighty", "nimble", "noble", "odd", "plucky", "proud", "quiet",
	"rapid", "rustic", "shiny", "silent", "silver", "sleepy", "smart", "snappy", "soft", "solar",
	"spry", "steady", "sunny", "swift", "tidy", "tiny", "vivid", "warm", "wild", "witty",
}

var animals = []string{
	"otter", "falcon", "badger", "beaver", "bison", "cobra", "condor", "coyote", "crane", "dingo",
	"dolphin", "eagle", "ferret", "finch", "fox", "gecko", "heron", "hedgehog", "ibis", "jaguar",
	"koala", "lemur", "lynx", "marmot", "moose", "newt", "ocelot", "orca", "owl", "panda",
	"parrot", "pelican", "puffin", "quail", "rabbit", "raven", "seal", "sparrow", "squid", "stork",
	"tiger", "toucan", "turtle", "walrus", "weasel", "whale", "wombat", "yak", "zebra", "robin",
}

// RandomName returns "<adjective>-<animal>-<10..99>", always valid per ValidateName.
func RandomName() string {
	return fmt.Sprintf("%s-%s-%d",
		adjectives[rand.IntN(len(adjectives))],
		animals[rand.IntN(len(animals))],
		10+rand.IntN(90))
}
