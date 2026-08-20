package service

import (
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"
)

// keysIn decodes every <key> element's text out of a rendered plist, in
// document order. It proves two things at once: that the document is
// well-formed XML (the decode loop runs all the way to io.EOF with no other
// error) and exactly which top-level dict keys made it into the output. A
// non-EOF error -- the shape a broken-out-of injection would produce --
// fails the test rather than silently truncating the key list.
func keysIn(t *testing.T, doc []byte) []string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(string(doc)))
	var keys []string
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("document did not parse as well-formed XML: %v", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var text string
		if err := dec.DecodeElement(&text, &start); err != nil {
			t.Fatalf("decode <key> element: %v", err)
		}
		keys = append(keys, text)
	}
	return keys
}

func mustReparse(t *testing.T, doc []byte) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(string(doc)))
	for {
		_, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Fatalf("re-parse rendered plist: %v", err)
		}
	}
}

func TestRenderPlistBasics(t *testing.T) {
	p := launchdPlist{
		Label:             Label,
		ProgramArguments:  []string{"/opt/agent-utils/bin/agent-utils", "--listen-addr", "127.0.0.1", "--listen-port", "8787", "listener", "start"},
		RunAtLoad:         true,
		KeepAlive:         true,
		StandardOutPath:   "/Users/me/.agent-utils/listener.stdout.log",
		StandardErrorPath: "/Users/me/.agent-utils/listener.stderr.log",
		WorkingDirectory:  "/Users/me/.agent-utils",
	}
	doc, err := renderPlist(p)
	if err != nil {
		t.Fatalf("renderPlist: %v", err)
	}
	mustReparse(t, doc)

	text := string(doc)
	if !strings.Contains(text, "<string>"+Label+"</string>") {
		t.Errorf("rendered plist missing label %q:\n%s", Label, text)
	}
	if got := p.ProgramArguments; got[len(got)-2] != "listener" || got[len(got)-1] != "start" {
		t.Fatalf("test fixture bug: args do not end with listener start: %v", got)
	}
	if !strings.Contains(text, "<key>ProgramArguments</key>") {
		t.Errorf("rendered plist missing ProgramArguments key:\n%s", text)
	}
	// The two arguments launchd must invoke last, rendered in order,
	// immediately preceding the array's close tag.
	last := "<string>listener</string>\n\t\t<string>start</string>\n\t</array>"
	if !strings.Contains(normalizeIndent(text), normalizeIndent(last)) {
		t.Errorf("ProgramArguments does not end with listener/start:\n%s", text)
	}
	// encoding/xml renders back-to-back start/end tokens with nothing
	// between them as an explicit empty element ("<true></true>"), not the
	// self-closing "<true/>" form -- the two are equivalent XML, and any
	// parser (including launchd's) reads them the same way.
	if !strings.Contains(text, "<key>RunAtLoad</key>") || !strings.Contains(text, "<true></true>") {
		t.Errorf("rendered plist does not show RunAtLoad true:\n%s", text)
	}

	if strings.Contains(text, "EnvironmentVariables") {
		t.Errorf("rendered plist must never contain EnvironmentVariables:\n%s", text)
	}
	if strings.Contains(text, "GITHUB_TOKEN") {
		t.Errorf("rendered plist must never contain GITHUB_TOKEN:\n%s", text)
	}

	keys := keysIn(t, doc)
	want := []string{"Label", "ProgramArguments", "RunAtLoad", "KeepAlive", "StandardOutPath", "StandardErrorPath", "WorkingDirectory"}
	if len(keys) != len(want) {
		t.Fatalf("got %d top-level keys %v, want %v", len(keys), keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("key[%d] = %q, want %q (full: %v)", i, keys[i], want[i], keys)
		}
	}
}

// normalizeIndent collapses runs of tabs/spaces so the "ends with listener
// start" assertion above does not depend on the encoder's exact indent
// width.
func normalizeIndent(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TestRenderPlistEscapesInjectionAttempt is the security-critical test: a
// caller-controlled --listen-addr value carrying XML metacharacters must
// come out escaped, never as literal markup. An unescaped '<' or
// '</string>' here would let a crafted --listen-addr value close the
// <string> element early and open a sibling <key>EnvironmentVariables</key>
// in a file launchd executes at every login.
func TestRenderPlistEscapesInjectionAttempt(t *testing.T) {
	malicious := `<>&"`
	injection := `</string></array><key>EnvironmentVariables</key><dict><key>GITHUB_TOKEN</key><string>x</string></dict><key>ProgramArguments</key><array><string>`
	p := launchdPlist{
		Label:             Label,
		ProgramArguments:  []string{"/opt/agent-utils/bin/agent-utils", "--listen-addr", malicious, "--listen-addr", injection, "listener", "start"},
		RunAtLoad:         true,
		KeepAlive:         true,
		StandardOutPath:   "/Users/me/.agent-utils/listener.stdout.log",
		StandardErrorPath: "/Users/me/.agent-utils/listener.stderr.log",
		WorkingDirectory:  "/Users/me/.agent-utils",
	}
	doc, err := renderPlist(p)
	if err != nil {
		t.Fatalf("renderPlist: %v", err)
	}
	mustReparse(t, doc)

	keys := keysIn(t, doc)
	for _, k := range keys {
		if k == "EnvironmentVariables" {
			t.Fatalf("injection succeeded: EnvironmentVariables key present, keys=%v\n%s", keys, doc)
		}
	}
	want := []string{"Label", "ProgramArguments", "RunAtLoad", "KeepAlive", "StandardOutPath", "StandardErrorPath", "WorkingDirectory"}
	if len(keys) != len(want) {
		t.Fatalf("injection added or removed keys: got %v, want %v\n%s", keys, want, doc)
	}

	// The raw bytes must never contain the injection payload's markup
	// literally -- if they did, the payload would have broken out of its
	// <string> element rather than staying inside it as escaped text. The
	// keys check above is the structural proof; this is the textual one.
	if strings.Contains(string(doc), "</string></array><key>EnvironmentVariables</key>") {
		t.Fatalf("injection payload appears as literal unescaped markup:\n%s", doc)
	}

	// Decoding unescapes entities, so the decoder should hand back the
	// original malicious string as ordinary element text -- that round trip
	// is what proves the bytes on the wire were the escaped form, not a
	// dropped or mangled value.
	dec := xml.NewDecoder(strings.NewReader(string(doc)))
	var found bool
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		cd, ok := tok.(xml.CharData)
		if !ok {
			continue
		}
		if string(cd) == malicious {
			found = true
		}
	}
	if !found {
		t.Fatalf("malicious value %q did not round-trip through the rendered document:\n%s", malicious, doc)
	}
}
