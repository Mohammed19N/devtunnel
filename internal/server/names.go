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

// ValidateName reports whether name is an allowed subdomain (syntax and
// reserved names only; see BlockedWord for the phishing blocklist). On failure it
// returns a *protocol.Error with CodeInvalidName.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return &protocol.Error{Code: protocol.CodeInvalidName,
			Message: "name must be 3-32 chars of a-z, 0-9 and '-', not starting or ending with '-'"}
	}
	if reserved[name] {
		return &protocol.Error{Code: protocol.CodeInvalidName, Message: fmt.Sprintf("name %q is reserved", name)}
	}
	return nil
}

// blockedWords are substrings that make a subdomain unavailable (phishing /
// brand-impersonation abuse). BlockedWord checks look-alike spellings too.
var blockedWords = []string{"paypal", "login", "signin", "logon", "bank", "secure", "verify", "wallet", "password", "apple", "google", "microsoft", "office365", "outlook", "amazon", "facebook", "instagram", "whatsapp", "netflix", "coinbase", "binance", "metamask",
	"stripe", "github", "gitlab", "bitbucket", "shopify", "slack", "twilio", "discord", "sendgrid", "mailgun", "square", "plaid", "hubspot", "zoom", "dropbox", "docusign", "adobe", "yahoo", "icloud", "steam", "roblox", "telegram", "tiktok", "twitter", "linkedin", "chase", "wellsfargo", "citibank", "hsbc", "venmo", "cashapp", "zelle", "usps", "fedex", "dhl"}

var (
	leetI = strings.NewReplacer("0", "o", "3", "e", "4", "a", "5", "s", "7", "t", "8", "b", "9", "g", "1", "i")
	leetL = strings.NewReplacer("0", "o", "3", "e", "4", "a", "5", "s", "7", "t", "8", "b", "9", "g", "1", "l")
	glyph = strings.NewReplacer("rn", "m", "vv", "w")
)

// normalForms returns the spellings of name that BlockedWord inspects.
func normalForms(name string) []string {
	name = strings.ToLower(name)
	plain := strings.ReplaceAll(name, "-", "")
	a, b := leetI.Replace(plain), leetL.Replace(plain)
	return []string{name, plain, a, b, glyph.Replace(a), glyph.Replace(b)}
}

// BlockedWord returns the first blocked word found in any normalised form of
// name (dashes removed, digits read as letters, "rn"/"vv" as "m"/"w"), or "".
func BlockedWord(name string) string {
	for _, f := range normalForms(name) {
		for _, w := range blockedWords {
			if strings.Contains(f, w) {
				return w
			}
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
