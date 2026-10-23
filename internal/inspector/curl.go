package inspector

import (
	"sort"
	"strings"
)

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var curlSkip = map[string]bool{
	"Host": true, "Content-Length": true, "Connection": true, "Accept-Encoding": true,
}

// CurlCommand renders e as a single-line POSIX-shell cURL command against base+URI
// (base = public URL, or "http://"+Host when empty).
func CurlCommand(e *Exchange, base string) string {
	r := e.Request
	if base == "" {
		base = "http://" + r.Host
	}
	base = strings.TrimRight(base, "/")
	var b strings.Builder
	if r.Method == "HEAD" {
		b.WriteString("curl --head " + shQuote(base+r.URI)) // -X HEAD waits for a body
	} else {
		b.WriteString("curl -X " + shQuote(r.Method) + " " + shQuote(base+r.URI))
	}
	keys := make([]string, 0, len(r.Header))
	for k := range r.Header {
		if curlSkip[k] || strings.HasPrefix(k, "X-Forwarded-") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range r.Header[k] {
			b.WriteString(" -H " + shQuote(k+": "+v))
		}
	}
	if len(r.Body.Data) > 0 {
		b.WriteString(" --data-binary " + shQuote(string(r.Body.Data)))
		if r.Body.Truncated {
			b.WriteString(" # body truncated")
		}
	}
	return b.String()
}
