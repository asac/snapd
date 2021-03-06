// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2015-2016 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package asserts

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// AssertionType describes a known assertion type with its name and metadata.
type AssertionType struct {
	// Name of the type.
	Name string
	// PrimaryKey holds the names of the headers that constitute the
	// unique primary key for this assertion type.
	PrimaryKey []string

	assembler func(assert assertionBase) (Assertion, error)
}

// Understood assertion types.
var (
	AccountType         = &AssertionType{"account", []string{"account-id"}, assembleAccount}
	AccountKeyType      = &AssertionType{"account-key", []string{"account-id", "public-key-id"}, assembleAccountKey}
	ModelType           = &AssertionType{"model", []string{"series", "brand-id", "model"}, assembleModel}
	SerialType          = &AssertionType{"serial", []string{"brand-id", "model", "serial"}, assembleSerial}
	SnapDeclarationType = &AssertionType{"snap-declaration", []string{"series", "snap-id"}, assembleSnapDeclaration}
	SnapBuildType       = &AssertionType{"snap-build", []string{"series", "snap-id", "snap-digest"}, assembleSnapBuild}
	SnapRevisionType    = &AssertionType{"snap-revision", []string{"series", "snap-id", "snap-digest"}, assembleSnapRevision}

// ...
)

var typeRegistry = map[string]*AssertionType{
	AccountType.Name:         AccountType,
	AccountKeyType.Name:      AccountKeyType,
	ModelType.Name:           ModelType,
	SerialType.Name:          SerialType,
	SnapDeclarationType.Name: SnapDeclarationType,
	SnapBuildType.Name:       SnapBuildType,
	SnapRevisionType.Name:    SnapRevisionType,
}

// Type returns the AssertionType with name or nil
func Type(name string) *AssertionType {
	return typeRegistry[name]
}

// Assertion represents an assertion through its general elements.
type Assertion interface {
	// Type returns the type of this assertion
	Type() *AssertionType
	// Revision returns the revision of this assertion
	Revision() int
	// AuthorityID returns the authority that signed this assertion
	AuthorityID() string

	// Header retrieves the header with name
	Header(name string) string

	// Headers returns the complete headers
	Headers() map[string]string

	// Body returns the body of this assertion
	Body() []byte

	// Signature returns the signed content and its unprocessed signature
	Signature() (content, signature []byte)
}

// MediaType is the media type for encoded assertions on the wire.
const MediaType = "application/x.ubuntu.assertion"

// assertionBase is the concrete base to hold representation data for actual assertions.
type assertionBase struct {
	headers map[string]string
	body    []byte
	// parsed revision
	revision int
	// preserved content
	content []byte
	// unprocessed signature
	signature []byte
}

// Type returns the assertion type.
func (ab *assertionBase) Type() *AssertionType {
	return Type(ab.headers["type"])
}

// Revision returns the assertion revision.
func (ab *assertionBase) Revision() int {
	return ab.revision
}

// AuthorityID returns the authority-id a.k.a the signer id of the assertion.
func (ab *assertionBase) AuthorityID() string {
	return ab.headers["authority-id"]
}

// Header returns the value of an header by name.
func (ab *assertionBase) Header(name string) string {
	return ab.headers[name]
}

// Headers returns the complete headers.
func (ab *assertionBase) Headers() map[string]string {
	res := make(map[string]string, len(ab.headers))
	for name, v := range ab.headers {
		res[name] = v
	}
	return res
}

// Body returns the body of the assertion.
func (ab *assertionBase) Body() []byte {
	return ab.body
}

// Signature returns the signed content and its unprocessed signature.
func (ab *assertionBase) Signature() (content, signature []byte) {
	return ab.content, ab.signature
}

// sanity check
var _ Assertion = (*assertionBase)(nil)

var (
	nl   = []byte("\n")
	nlnl = []byte("\n\n")

	// for basic sanity checking of header names
	headerNameSanity = regexp.MustCompile("^[a-z][a-z0-9-]*[a-z0-9]$")
)

func parseHeaders(head []byte) (map[string]string, error) {
	if !utf8.Valid(head) {
		return nil, fmt.Errorf("header is not utf8")
	}
	headers := make(map[string]string)
	lines := strings.Split(string(head), "\n")
	for i := 0; i < len(lines); {
		entry := lines[i]
		i++
		nameValueSplit := strings.Index(entry, ":")
		if nameValueSplit == -1 {
			return nil, fmt.Errorf("header entry missing ':' separator: %q", entry)
		}
		name := entry[:nameValueSplit]
		if !headerNameSanity.MatchString(name) {
			return nil, fmt.Errorf("invalid header name: %q", name)
		}

		afterSplit := nameValueSplit + 1
		if afterSplit == len(entry) {
			// multiline value
			size := 0
			j := i
			for j < len(lines) {
				iline := lines[j]
				if len(iline) == 0 || iline[0] != ' ' {
					break
				}
				size += len(iline)
				j++
			}
			if j == i {
				return nil, fmt.Errorf("empty multiline header value: %q", entry)
			}

			valueBuf := bytes.NewBuffer(make([]byte, 0, size-1))
			valueBuf.WriteString(lines[i][1:])
			i++
			for i < j {
				valueBuf.WriteByte('\n')
				valueBuf.WriteString(lines[i][1:])
				i++
			}

			headers[name] = valueBuf.String()
			continue
		}

		if entry[afterSplit] != ' ' {
			return nil, fmt.Errorf("header entry should have a space or newline (multiline) before value: %q", entry)
		}

		headers[name] = entry[afterSplit+1:]
	}
	return headers, nil
}

// Decode parses a serialized assertion.
//
// The expected serialisation format looks like:
//
//   HEADER ("\n\n" BODY?)? "\n\n" SIGNATURE
//
// where:
//
//    HEADER is a set of header entries separated by "\n"
//    BODY can be arbitrary,
//    SIGNATURE is the signature
//
// A header entry for a single line value (no '\n' in it) looks like:
//
//   NAME ": " VALUE
//
// A header entry for a multiline value (a value with '\n's in it) looks like:
//
//   NAME ":\n"  1-space indented VALUE
//
// The following headers are mandatory:
//
//   type
//   authority-id (the signer id)
//
// Further for a given assertion type all the primary key headers
// must be non empty and must not contain '/'.
//
// The following headers expect integer values and if omitted
// otherwise are assumed to be 0:
//
//   revision (a positive int)
//   body-length (expected to be equal to the length of BODY)
//
// Typically list values in headers are expected to be comma separated.
// Times are expected to be in the RFC3339 format: "2006-01-02T15:04:05Z07:00".
func Decode(serializedAssertion []byte) (Assertion, error) {
	// copy to get an independent backstorage that can't be mutated later
	assertionSnapshot := make([]byte, len(serializedAssertion))
	copy(assertionSnapshot, serializedAssertion)
	contentSignatureSplit := bytes.LastIndex(assertionSnapshot, nlnl)
	if contentSignatureSplit == -1 {
		return nil, fmt.Errorf("assertion content/signature separator not found")
	}
	content := assertionSnapshot[:contentSignatureSplit]
	signature := assertionSnapshot[contentSignatureSplit+2:]

	headersBodySplit := bytes.Index(content, nlnl)
	var body, head []byte
	if headersBodySplit == -1 {
		head = content
	} else {
		body = content[headersBodySplit+2:]
		if len(body) == 0 {
			body = nil
		}
		head = content[:headersBodySplit]
	}

	headers, err := parseHeaders(head)
	if err != nil {
		return nil, fmt.Errorf("parsing assertion headers: %v", err)
	}

	return Assemble(headers, body, content, signature)
}

// Maximum assertion component sizes.
const (
	MaxBodySize      = 2 * 1024 * 1024
	MaxHeadersSize   = 128 * 1024
	MaxSignatureSize = 128 * 1024
)

// Decoder parses a stream of assertions bundled by separating them with double newlines.
type Decoder struct {
	rd             io.Reader
	initialBufSize int
	b              *bufio.Reader
	err            error
	maxHeadersSize int
	maxBodySize    int
	maxSigSize     int
}

// initBuffer finishes a Decoder initialization by setting up the bufio.Reader,
// it returns the *Decoder for convenience of notation.
func (d *Decoder) initBuffer() *Decoder {
	d.b = bufio.NewReaderSize(d.rd, d.initialBufSize)
	return d
}

const defaultDecoderButSize = 4096

// NewDecoder returns a Decoder to parse the stream of assertions from the reader.
func NewDecoder(r io.Reader) *Decoder {
	return (&Decoder{
		rd:             r,
		initialBufSize: defaultDecoderButSize,
		maxHeadersSize: MaxHeadersSize,
		maxBodySize:    MaxBodySize,
		maxSigSize:     MaxSignatureSize,
	}).initBuffer()
}

func (d *Decoder) peek(size int) ([]byte, error) {
	buf, err := d.b.Peek(size)
	if err == bufio.ErrBufferFull {
		rebuf, reerr := d.b.Peek(d.b.Buffered())
		if reerr != nil {
			panic(reerr)
		}
		mr := io.MultiReader(bytes.NewBuffer(rebuf), d.rd)
		d.b = bufio.NewReaderSize(mr, (size/d.initialBufSize+1)*d.initialBufSize)
		buf, err = d.b.Peek(size)
	}
	if err != nil && d.err == nil {
		d.err = err
	}
	return buf, d.err
}

// NB: readExact and readUntil use peek underneath and their returned
// buffers are valid only until the next reading call

func (d *Decoder) readExact(size int) ([]byte, error) {
	buf, err := d.peek(size)
	d.b.Discard(len(buf))
	if len(buf) == size {
		return buf, nil
	}
	if err == io.EOF {
		return buf, io.ErrUnexpectedEOF
	}
	return buf, err
}

func (d *Decoder) readUntil(delim []byte, maxSize int) ([]byte, error) {
	last := 0
	size := d.initialBufSize
	for {
		buf, err := d.peek(size)
		if i := bytes.Index(buf[last:], delim); i >= 0 {
			d.b.Discard(last + i + len(delim))
			return buf[:last+i+len(delim)], nil
		}
		// report errors only once we have consumed what is buffered
		if err != nil && len(buf) == d.b.Buffered() {
			d.b.Discard(len(buf))
			return buf, err
		}
		last = size - len(delim) + 1
		size *= 2
		if size > maxSize {
			return nil, fmt.Errorf("maximum size exceeded while looking for delimiter %q", delim)
		}
	}
}

// Decode parses the next assertion from the stream.
// It returns the error io.EOF at the end of a well-formed stream.
func (d *Decoder) Decode() (Assertion, error) {
	// read the headers and the nlnl separator after them
	headAndSep, err := d.readUntil(nlnl, d.maxHeadersSize)
	if err != nil {
		if err == io.EOF {
			if len(headAndSep) != 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, io.EOF
		}
		return nil, fmt.Errorf("error reading assertion headers: %v", err)
	}

	headLen := len(headAndSep) - len(nlnl)
	headers, err := parseHeaders(headAndSep[:headLen])
	if err != nil {
		return nil, fmt.Errorf("parsing assertion headers: %v", err)
	}

	length, err := checkInteger(headers, "body-length", 0)
	if err != nil {
		return nil, fmt.Errorf("assertion: %v", err)
	}
	if length > d.maxBodySize {
		return nil, fmt.Errorf("assertion body length %d exceeds maximum body size", length)
	}

	// save the headers before we try to read more, and setup to capture
	// the whole content in a buffer
	contentBuf := bytes.NewBuffer(make([]byte, 0, len(headAndSep)+length))
	contentBuf.Write(headAndSep)

	if length > 0 {
		// read the body if length != 0
		body, err := d.readExact(length)
		if err != nil {
			return nil, err
		}
		contentBuf.Write(body)
	}

	// try to read the end of body a.k.a content/signature separator
	endOfBody, err := d.readUntil(nlnl, d.maxSigSize)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("error reading assertion trailer: %v", err)
	}

	var sig []byte
	if bytes.Equal(endOfBody, nlnl) {
		// we got the nlnl content/signature separator, read the signature now and the assertion/assertion nlnl separation
		sig, err = d.readUntil(nlnl, d.maxSigSize)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("error reading assertion signature: %v", err)
		}
	} else {
		// we got the signature directly which is a ok format only if body length == 0
		if length > 0 {
			return nil, fmt.Errorf("missing content/signature separator")
		}
		sig = endOfBody
		contentBuf.Truncate(headLen)
	}

	// normalize sig ending newlines
	if bytes.HasSuffix(sig, nlnl) {
		sig = sig[:len(sig)-1]
	}

	finalContent := contentBuf.Bytes()
	var finalBody []byte
	if length > 0 {
		finalBody = finalContent[headLen+len(nlnl):]
	}

	finalSig := make([]byte, len(sig))
	copy(finalSig, sig)

	return Assemble(headers, finalBody, finalContent, finalSig)
}

func checkRevision(headers map[string]string) (int, error) {
	revision, err := checkInteger(headers, "revision", 0)
	if err != nil {
		return -1, err
	}
	if revision < 0 {
		return -1, fmt.Errorf("revision should be positive: %v", revision)
	}
	return revision, nil
}

// Assemble assembles an assertion from its components.
func Assemble(headers map[string]string, body, content, signature []byte) (Assertion, error) {
	length, err := checkInteger(headers, "body-length", 0)
	if err != nil {
		return nil, fmt.Errorf("assertion: %v", err)
	}
	if length != len(body) {
		return nil, fmt.Errorf("assertion body length and declared body-length don't match: %v != %v", len(body), length)
	}

	if _, err := checkNotEmpty(headers, "authority-id"); err != nil {
		return nil, fmt.Errorf("assertion: %v", err)
	}

	typ, err := checkNotEmpty(headers, "type")
	if err != nil {
		return nil, fmt.Errorf("assertion: %v", err)
	}
	assertType := Type(typ)
	if assertType == nil {
		return nil, fmt.Errorf("unknown assertion type: %q", typ)
	}

	for _, primKey := range assertType.PrimaryKey {
		if _, err := checkPrimaryKey(headers, primKey); err != nil {
			return nil, fmt.Errorf("assertion %s: %v", assertType.Name, err)
		}
	}

	revision, err := checkRevision(headers)
	if err != nil {
		return nil, fmt.Errorf("assertion: %v", err)
	}

	if len(signature) == 0 {
		return nil, fmt.Errorf("empty assertion signature")
	}

	assert, err := assertType.assembler(assertionBase{
		headers:   headers,
		body:      body,
		revision:  revision,
		content:   content,
		signature: signature,
	})
	if err != nil {
		return nil, fmt.Errorf("assertion %s: %v", assertType.Name, err)
	}
	return assert, nil
}

func writeHeader(buf *bytes.Buffer, headers map[string]string, name string) {
	buf.WriteByte('\n')
	buf.WriteString(name)
	value := headers[name]
	if strings.IndexRune(value, '\n') != -1 {
		// multiline value => quote by 1-space indenting
		buf.WriteString(":\n ")
		value = strings.Replace(value, "\n", "\n ", -1)
	} else {
		buf.WriteString(": ")
	}
	buf.WriteString(value)
}

func assembleAndSign(assertType *AssertionType, headers map[string]string, body []byte, privKey PrivateKey) (Assertion, error) {
	err := checkAssertType(assertType)
	if err != nil {
		return nil, err
	}

	finalHeaders := make(map[string]string, len(headers))
	for name, value := range headers {
		finalHeaders[name] = value
	}
	bodyLength := len(body)
	finalBody := make([]byte, bodyLength)
	copy(finalBody, body)
	finalHeaders["type"] = assertType.Name
	finalHeaders["body-length"] = strconv.Itoa(bodyLength)

	if _, err := checkNotEmpty(finalHeaders, "authority-id"); err != nil {
		return nil, err
	}

	revision, err := checkRevision(finalHeaders)
	if err != nil {
		return nil, err
	}

	buf := bytes.NewBufferString("type: ")
	buf.WriteString(assertType.Name)

	writeHeader(buf, finalHeaders, "authority-id")
	if revision > 0 {
		writeHeader(buf, finalHeaders, "revision")
	} else {
		delete(finalHeaders, "revision")
	}
	written := map[string]bool{
		"type":         true,
		"authority-id": true,
		"revision":     true,
		"body-length":  true,
	}
	for _, primKey := range assertType.PrimaryKey {
		if _, err := checkPrimaryKey(finalHeaders, primKey); err != nil {
			return nil, err
		}
		writeHeader(buf, finalHeaders, primKey)
		written[primKey] = true
	}

	// emit other headers in lexicographic order
	otherKeys := make([]string, 0, len(finalHeaders))
	for name := range finalHeaders {
		if !written[name] {
			otherKeys = append(otherKeys, name)
		}
	}
	sort.Strings(otherKeys)
	for _, k := range otherKeys {
		writeHeader(buf, finalHeaders, k)
	}

	// body-length and body
	if bodyLength > 0 {
		writeHeader(buf, finalHeaders, "body-length")
	} else {
		delete(finalHeaders, "body-length")
	}
	if bodyLength > 0 {
		buf.Grow(bodyLength + 2)
		buf.Write(nlnl)
		buf.Write(finalBody)
	} else {
		finalBody = nil
	}
	content := buf.Bytes()

	signature, err := signContent(content, privKey)
	if err != nil {
		return nil, fmt.Errorf("cannot sign assertion: %v", err)
	}
	// be 'cat' friendly, add a ignored newline to the signature which is the last part of the encoded assertion
	signature = append(signature, '\n')

	assert, err := assertType.assembler(assertionBase{
		headers:   finalHeaders,
		body:      finalBody,
		revision:  revision,
		content:   content,
		signature: signature,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot assemble assertion %s: %v", assertType.Name, err)
	}
	return assert, nil
}

// Encode serializes an assertion.
func Encode(assert Assertion) []byte {
	content, signature := assert.Signature()
	needed := len(content) + 2 + len(signature)
	buf := bytes.NewBuffer(make([]byte, 0, needed))
	buf.Write(content)
	buf.Write(nlnl)
	buf.Write(signature)
	return buf.Bytes()
}

// Encoder emits a stream of assertions bundled by separating them with double newlines.
type Encoder struct {
	wr      io.Writer
	nextSep []byte
}

// NewEncoder returns a Encoder to emit a stream of assertions to a writer.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{wr: w}
}

// append emits an already encoded assertion into the stream with a proper required separator.
func (enc *Encoder) append(encoded []byte) error {
	sz := len(encoded)
	if sz == 0 {
		return fmt.Errorf("internal error: encoded assertion cannot be empty")
	}

	_, err := enc.wr.Write(enc.nextSep)
	if err != nil {
		return err
	}

	_, err = enc.wr.Write(encoded)
	if err != nil {
		return err
	}

	if encoded[sz-1] != '\n' {
		_, err = enc.wr.Write(nl)
		if err != nil {
			return err
		}
	}
	enc.nextSep = nl

	return nil
}

// Encode emits the assertion into the stream with the required separator.
// Errors here are always about writing given that Encode() itself cannot error.
func (enc *Encoder) Encode(assert Assertion) error {
	encoded := Encode(assert)
	return enc.append(encoded)
}
