package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
)

// launchdPlist is the subset of an Apple property list this program writes
// for its launchd user agent. It deliberately holds no EnvironmentVariables
// key and no secret: the daemon reads its token from ~/.agent-utils/env,
// which stays out of a file that (a) launchd loads into every session and
// (b) sits on disk at mode 0644, world readable.
type launchdPlist struct {
	Label             string
	ProgramArguments  []string
	RunAtLoad         bool
	KeepAlive         bool
	StandardOutPath   string
	StandardErrorPath string
	WorkingDirectory  string
}

// renderPlist renders p as a complete Apple property list document.
//
// This goes through encoding/xml end to end, never string concatenation or
// fmt.Sprintf into a template. ProgramArguments carries the caller's
// --listen-addr and --listen-port values, which reach here unsanitized; a
// hand-built template that missed escaping a single '<' or forgot to close
// a '</string>' early would let one of those values terminate the element
// and open a sibling key of the attacker's choosing -- including the very
// EnvironmentVariables key this struct is designed to omit -- in a file
// launchd executes at every login. encoding/xml's encoder escapes every
// piece of text it is given, so there is no template text to get wrong.
func renderPlist(p launchdPlist) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	// The DOCTYPE is not itself untrusted-data-bearing (it carries no
	// caller input), so a literal write here does not reopen the injection
	// this function otherwise closes.
	buf.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")

	enc := xml.NewEncoder(&buf)
	enc.Indent("", "\t")

	root := xml.StartElement{
		Name: xml.Name{Local: "plist"},
		Attr: []xml.Attr{{Name: xml.Name{Local: "version"}, Value: "1.0"}},
	}
	// EncodeElement, not Encode: p implements xml.Marshaler and takes full
	// responsibility for its own <dict> body, so the start element passed
	// here only establishes the surrounding <plist version="1.0"> wrapper.
	if err := enc.EncodeElement(dictBody(p), root); err != nil {
		return nil, fmt.Errorf("encode plist: %w", err)
	}
	if err := enc.Flush(); err != nil {
		return nil, fmt.Errorf("flush plist encoder: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// dictBody adapts launchdPlist to xml.Marshaler so renderPlist can hand it
// straight to the encoder.
type dictBody launchdPlist

// MarshalXML writes p as a plist <dict>: a flat sequence of <key> elements
// each immediately followed by its value element. That shape has no natural
// Go struct encoding (encoding/xml has no concept of an interleaved
// key/value list, and a real map would drop the field order the plist DTD
// and human readers both expect), so this writes the token stream by hand.
// Every piece of text still passes through EncodeElement, which is what
// performs the escaping renderPlist's doc comment depends on -- the manual
// token stream controls STRUCTURE, never raw bytes of caller-supplied TEXT.
func (p dictBody) MarshalXML(e *xml.Encoder, _ xml.StartElement) error {
	dict := xml.StartElement{Name: xml.Name{Local: "dict"}}
	if err := e.EncodeToken(dict); err != nil {
		return err
	}

	if err := encodeKeyString(e, "Label", p.Label); err != nil {
		return err
	}
	if err := encodeKeyStringArray(e, "ProgramArguments", p.ProgramArguments); err != nil {
		return err
	}
	if err := encodeKeyBool(e, "RunAtLoad", p.RunAtLoad); err != nil {
		return err
	}
	if err := encodeKeyBool(e, "KeepAlive", p.KeepAlive); err != nil {
		return err
	}
	if err := encodeKeyString(e, "StandardOutPath", p.StandardOutPath); err != nil {
		return err
	}
	if err := encodeKeyString(e, "StandardErrorPath", p.StandardErrorPath); err != nil {
		return err
	}
	if err := encodeKeyString(e, "WorkingDirectory", p.WorkingDirectory); err != nil {
		return err
	}

	return e.EncodeToken(dict.End())
}

// encodeKeyString writes <key>name</key><string>value</string>.
// EncodeElement is what escapes value's text; that is the whole point of
// building this document through encoding/xml instead of a template.
func encodeKeyString(e *xml.Encoder, name, value string) error {
	if err := e.EncodeElement(name, xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
		return err
	}
	return e.EncodeElement(value, xml.StartElement{Name: xml.Name{Local: "string"}})
}

// encodeKeyStringArray writes <key>name</key><array>...</array>, one
// <string> per value, each escaped the same way encodeKeyString escapes a
// scalar.
func encodeKeyStringArray(e *xml.Encoder, name string, values []string) error {
	if err := e.EncodeElement(name, xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
		return err
	}
	arr := xml.StartElement{Name: xml.Name{Local: "array"}}
	if err := e.EncodeToken(arr); err != nil {
		return err
	}
	for _, v := range values {
		if err := e.EncodeElement(v, xml.StartElement{Name: xml.Name{Local: "string"}}); err != nil {
			return err
		}
	}
	return e.EncodeToken(arr.End())
}

// encodeKeyBool writes <key>name</key><true/> or <key>name</key><false/>,
// the plist DTD's boolean form: an empty element, never text content.
// Back-to-back start/end tokens with nothing between them render as an
// empty element, which any XML parser -- launchd's included -- treats
// identically to a self-closing tag.
func encodeKeyBool(e *xml.Encoder, name string, value bool) error {
	if err := e.EncodeElement(name, xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
		return err
	}
	local := "false"
	if value {
		local = "true"
	}
	tok := xml.StartElement{Name: xml.Name{Local: local}}
	if err := e.EncodeToken(tok); err != nil {
		return err
	}
	return e.EncodeToken(tok.End())
}
