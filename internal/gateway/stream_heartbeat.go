package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	streamHeartbeatComment     = ": keepalive\n\n"
	maxEncodedStreamKeyLength  = len("stream") * len(`\u0000`)
	maxCapturedPrimitiveLength = len("true") + 1
)

type heartbeatWriterContextKey struct{}

var streamHeartbeatPaths = map[string]struct{}{
	"/v3/chat/completions": {},
	"/v3/completions":      {},
	"/v3/responses":        {},
}

func supportsStreamHeartbeat(request *http.Request) bool {
	if request.Method != http.MethodPost {
		return false
	}
	_, ok := streamHeartbeatPaths[request.URL.Path]
	return ok
}

// streamDetectingBody observes the request body as the HTTP transport sends it
// to OVMS. It does not buffer the payload, which keeps large base64 image
// requests bounded by the gateway's existing request-body limit rather than
// duplicating them in memory.
type streamDetectingBody struct {
	io.ReadCloser
	scanner  streamFlagScanner
	onResult func(bool)
	once     sync.Once
}

func newStreamDetectingBody(body io.ReadCloser, onResult func(bool)) io.ReadCloser {
	return &streamDetectingBody{
		ReadCloser: body,
		onResult:   onResult,
	}
}

func (body *streamDetectingBody) Read(buffer []byte) (int, error) {
	n, err := body.ReadCloser.Read(buffer)
	if n > 0 {
		body.scanner.feed(buffer[:n])
	}
	if err != nil {
		body.finish(errors.Is(err, io.EOF) && body.scanner.streaming())
	}
	return n, err
}

func (body *streamDetectingBody) Close() error {
	body.finish(false)
	return body.ReadCloser.Close()
}

func (body *streamDetectingBody) finish(streaming bool) {
	body.once.Do(func() {
		body.onResult(streaming)
	})
}

type topLevelState uint8

const (
	expectRoot topLevelState = iota
	expectFirstKey
	expectKey
	expectColon
	expectValue
	skipValue
	rootComplete
)

// streamFlagScanner recognizes the final top-level "stream" boolean without
// decoding or retaining other JSON values. In particular, base64 image strings
// are scanned in place and never copied.
type streamFlagScanner struct {
	depth int
	state topLevelState

	inString    bool
	escaped     bool
	captureKey  bool
	keyOverflow bool
	key         []byte
	streamKey   bool
	inPrimitive bool
	primitive   []byte
	primitiveWS bool

	streamSeen bool
	stream     bool
	malformed  bool
}

func (scanner *streamFlagScanner) feed(data []byte) {
	for _, value := range data {
		if scanner.malformed {
			return
		}
		if scanner.state == rootComplete {
			if !isJSONWhitespace(value) {
				scanner.malformed = true
			}
			continue
		}
		if scanner.inString {
			scanner.feedString(value)
			continue
		}
		if scanner.inPrimitive && scanner.primitiveWS && !isJSONWhitespace(value) && value != ',' && value != '}' {
			scanner.malformed = true
			continue
		}
		if isJSONWhitespace(value) {
			if scanner.inPrimitive {
				scanner.primitiveWS = true
			}
			continue
		}
		if scanner.state == expectRoot {
			if value != '{' {
				scanner.malformed = true
				continue
			}
			scanner.depth = 1
			scanner.state = expectFirstKey
			continue
		}

		if scanner.depth == 1 {
			if scanner.feedTopLevel(value) {
				continue
			}
		}

		switch value {
		case '{', '[':
			scanner.depth++
		case '}', ']':
			if scanner.depth <= 1 {
				scanner.malformed = true
				continue
			}
			scanner.depth--
		case '"':
			scanner.inString = true
			scanner.captureKey = false
		}
	}
}

// feedTopLevel returns true when value has been fully handled.
func (scanner *streamFlagScanner) feedTopLevel(value byte) bool {
	switch scanner.state {
	case expectFirstKey:
		switch value {
		case '"':
			scanner.inString = true
			scanner.captureKey = true
			scanner.keyOverflow = false
			scanner.key = scanner.key[:0]
		case '}':
			scanner.finishRoot()
		default:
			scanner.malformed = true
		}
		return true
	case expectKey:
		if value != '"' {
			scanner.malformed = true
			return true
		}
		scanner.inString = true
		scanner.captureKey = true
		scanner.keyOverflow = false
		scanner.key = scanner.key[:0]
		return true
	case expectColon:
		if value != ':' {
			scanner.malformed = true
		} else {
			scanner.state = expectValue
		}
		return true
	case expectValue:
		switch value {
		case '"':
			scanner.markNonBooleanStreamValue()
			scanner.inString = true
			scanner.captureKey = false
			scanner.state = skipValue
		case '{', '[':
			scanner.markNonBooleanStreamValue()
			scanner.depth++
			scanner.state = skipValue
		case ',', '}':
			scanner.malformed = true
		default:
			scanner.inPrimitive = true
			scanner.primitive = scanner.primitive[:0]
			scanner.appendPrimitive(value)
			scanner.primitiveWS = false
			scanner.state = skipValue
		}
		return true
	case skipValue:
		if scanner.inPrimitive {
			switch value {
			case ',':
				scanner.finishPrimitive()
				scanner.state = expectKey
			case '}':
				scanner.finishPrimitive()
				scanner.finishRoot()
			default:
				scanner.appendPrimitive(value)
			}
			return true
		}
		switch value {
		case ',':
			scanner.streamKey = false
			scanner.state = expectKey
		case '}':
			scanner.finishRoot()
		default:
			scanner.malformed = true
		}
		return true
	default:
		scanner.malformed = true
		return true
	}
}

func (scanner *streamFlagScanner) feedString(value byte) {
	if scanner.escaped {
		scanner.escaped = false
		if scanner.captureKey {
			scanner.appendKeyByte(value)
		}
		return
	}
	if value == '\\' {
		scanner.escaped = true
		if scanner.captureKey {
			scanner.appendKeyByte(value)
		}
		return
	}
	if value != '"' {
		if scanner.captureKey {
			scanner.appendKeyByte(value)
		}
		return
	}

	scanner.inString = false
	if !scanner.captureKey {
		return
	}
	scanner.captureKey = false
	scanner.streamKey = scanner.keyIsStream()
	scanner.state = expectColon
}

func (scanner *streamFlagScanner) appendKeyByte(value byte) {
	if len(scanner.key) < maxEncodedStreamKeyLength {
		scanner.key = append(scanner.key, value)
		return
	}
	scanner.keyOverflow = true
}

func (scanner *streamFlagScanner) keyIsStream() bool {
	if scanner.keyOverflow {
		return false
	}
	const target = "stream"
	targetOffset := 0
	for encodedOffset := 0; encodedOffset < len(scanner.key); {
		if targetOffset == len(target) {
			return false
		}

		value := scanner.key[encodedOffset]
		encodedOffset++
		if value == '\\' {
			if encodedOffset+5 > len(scanner.key) || scanner.key[encodedOffset] != 'u' {
				return false
			}
			encodedOffset++
			decoded, ok := decodeHexQuad(scanner.key[encodedOffset : encodedOffset+4])
			if !ok || decoded > 0x7f {
				return false
			}
			value = byte(decoded)
			encodedOffset += 4
		}
		if value != target[targetOffset] {
			return false
		}
		targetOffset++
	}
	return targetOffset == len(target)
}

func decodeHexQuad(encoded []byte) (uint16, bool) {
	var decoded uint16
	for _, value := range encoded {
		nibble, ok := hexNibble(value)
		if !ok {
			return 0, false
		}
		decoded = decoded<<4 | uint16(nibble)
	}
	return decoded, true
}

func hexNibble(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func (scanner *streamFlagScanner) appendPrimitive(value byte) {
	if scanner.streamKey && len(scanner.primitive) < maxCapturedPrimitiveLength {
		scanner.primitive = append(scanner.primitive, value)
	}
}

func (scanner *streamFlagScanner) finishPrimitive() {
	if scanner.streamKey {
		scanner.streamSeen = true
		scanner.stream = string(scanner.primitive) == "true"
	}
	scanner.streamKey = false
	scanner.inPrimitive = false
	scanner.primitive = scanner.primitive[:0]
	scanner.primitiveWS = false
}

func (scanner *streamFlagScanner) markNonBooleanStreamValue() {
	if scanner.streamKey {
		scanner.streamSeen = true
		scanner.stream = false
	}
	scanner.streamKey = false
}

func (scanner *streamFlagScanner) finishRoot() {
	if scanner.depth != 1 {
		scanner.malformed = true
		return
	}
	scanner.depth = 0
	scanner.state = rootComplete
}

func (scanner *streamFlagScanner) streaming() bool {
	return !scanner.malformed &&
		scanner.state == rootComplete &&
		scanner.streamSeen &&
		scanner.stream
}

func isJSONWhitespace(value byte) bool {
	switch value {
	case ' ', '\t', '\r', '\n':
		return true
	default:
		return false
	}
}

// heartbeatResponseWriter serializes OVMS response writes and SSE heartbeat
// comments onto the client connection. Its private header map keeps reverse
// proxy header mutations separate from a heartbeat that may need to commit an
// SSE response before OVMS produces response headers.
type heartbeatResponseWriter struct {
	writer          http.ResponseWriter
	header          http.Header
	heartbeatHeader http.Header
	context         context.Context
	cancel          context.CancelFunc
	interval        time.Duration

	writeMu             sync.Mutex
	committed           bool
	eventStream         bool
	backendHeadersReady bool
	atLineStart         bool
	atEventBoundary     bool
	sseLineHasData      bool
	ssePendingCR        bool
	lastBackendActivity time.Time

	lifecycleMu      sync.Mutex
	enabled          bool
	heartbeatAllowed bool
	stopped          bool
	stopChannel      chan struct{}
	activity         chan struct{}
	waitGroup        sync.WaitGroup
}

func newHeartbeatResponseWriter(
	writer http.ResponseWriter,
	ctx context.Context,
	cancel context.CancelFunc,
	interval time.Duration,
) *heartbeatResponseWriter {
	initialHeader := writer.Header().Clone()
	return &heartbeatResponseWriter{
		writer:           writer,
		header:           initialHeader.Clone(),
		heartbeatHeader:  initialHeader,
		context:          ctx,
		cancel:           cancel,
		interval:         interval,
		atLineStart:      true,
		atEventBoundary:  true,
		heartbeatAllowed: true,
		stopChannel:      make(chan struct{}),
		activity:         make(chan struct{}, 1),
	}
}

func (writer *heartbeatResponseWriter) Header() http.Header {
	return writer.header
}

func (writer *heartbeatResponseWriter) WriteHeader(status int) {
	writer.writeMu.Lock()
	defer writer.writeMu.Unlock()
	if writer.committed {
		return
	}
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		copyHeaders(writer.writer.Header(), writer.header)
		writer.writer.WriteHeader(status)
		clear(writer.writer.Header())
		return
	}
	writer.commitBackendHeadersLocked(status)
}

func (writer *heartbeatResponseWriter) Write(data []byte) (int, error) {
	writer.writeMu.Lock()
	defer writer.writeMu.Unlock()
	if !writer.committed {
		writer.commitBackendHeadersLocked(http.StatusOK)
	}
	n, err := writer.writer.Write(data)
	if n > 0 {
		writer.observeBackendSSELocked(data[:n])
		writer.lastBackendActivity = time.Now()
		select {
		case writer.activity <- struct{}{}:
		default:
		}
	}
	if err != nil {
		writer.cancel()
	}
	return n, err
}

func (writer *heartbeatResponseWriter) observeBackendSSELocked(data []byte) {
	for _, value := range data {
		if writer.ssePendingCR {
			writer.ssePendingCR = false
			if value == '\n' {
				continue
			}
		}
		switch value {
		case '\r':
			writer.finishSSELineLocked()
			writer.ssePendingCR = true
		case '\n':
			writer.finishSSELineLocked()
		default:
			writer.sseLineHasData = true
			writer.atLineStart = false
			writer.atEventBoundary = false
		}
	}
}

func (writer *heartbeatResponseWriter) finishSSELineLocked() {
	writer.atLineStart = true
	writer.atEventBoundary = !writer.sseLineHasData
	writer.sseLineHasData = false
}

func (writer *heartbeatResponseWriter) Flush() {
	_ = writer.FlushError()
}

func (writer *heartbeatResponseWriter) FlushError() error {
	writer.writeMu.Lock()
	defer writer.writeMu.Unlock()
	if !writer.committed {
		writer.commitBackendHeadersLocked(http.StatusOK)
	}
	err := http.NewResponseController(writer.writer).Flush()
	if err != nil {
		writer.cancel()
	}
	return err
}

func (writer *heartbeatResponseWriter) commitBackendHeadersLocked(status int) {
	copyHeaders(writer.writer.Header(), writer.header)
	writer.writer.WriteHeader(status)
	writer.committed = true
	writer.backendHeadersReady = false
	contentType, _, _ := mime.ParseMediaType(writer.header.Get("Content-Type"))
	writer.eventStream = status >= 200 && status < 300 && contentType == "text/event-stream"
}

func (writer *heartbeatResponseWriter) commitHeartbeatLocked() {
	headers := writer.writer.Header()
	copyHeaders(headers, writer.heartbeatHeader)
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	writer.writer.WriteHeader(http.StatusOK)
	writer.committed = true
	writer.eventStream = true
}

// prepareHeartbeatResponse runs before ReverseProxy copies backend headers into
// Header(). It temporarily suppresses heartbeat header reads so that the
// reverse proxy and heartbeat goroutine never access the header map
// concurrently.
func prepareHeartbeatResponse(response *http.Response) error {
	writer, ok := response.Request.Context().Value(heartbeatWriterContextKey{}).(*heartbeatResponseWriter)
	if !ok {
		return nil
	}
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode >= http.StatusOK &&
		response.StatusCode < http.StatusMultipleChoices &&
		contentType == "text/event-stream" {
		response.Header.Del("Content-Length")
		response.ContentLength = -1
	}
	contentEncoding := strings.TrimSpace(response.Header.Get("Content-Encoding"))
	if contentEncoding != "" &&
		!strings.EqualFold(contentEncoding, "identity") &&
		writer.disableHeartbeatForEncoding() {
		return fmt.Errorf(
			"backend returned Content-Encoding %q for an SSE heartbeat request",
			response.Header.Get("Content-Encoding"),
		)
	}
	writer.writeMu.Lock()
	defer writer.writeMu.Unlock()
	if writer.committed {
		if response.StatusCode < http.StatusOK ||
			response.StatusCode >= http.StatusMultipleChoices ||
			contentType != "text/event-stream" ||
			(contentEncoding != "" && !strings.EqualFold(contentEncoding, "identity")) {
			return fmt.Errorf(
				"backend returned %s with Content-Type %q and Content-Encoding %q after SSE heartbeat",
				response.Status,
				response.Header.Get("Content-Type"),
				response.Header.Get("Content-Encoding"),
			)
		}
		return nil
	}
	writer.backendHeadersReady = true
	return nil
}

// disableHeartbeatForEncoding closes the ordering gap where backend response
// headers can arrive before the transport finishes reading the request body.
// If stream detection has not enabled heartbeats yet, a later enable becomes a
// no-op. If it has, the caller must reject the incompatible response.
func (writer *heartbeatResponseWriter) disableHeartbeatForEncoding() bool {
	writer.lifecycleMu.Lock()
	defer writer.lifecycleMu.Unlock()
	writer.heartbeatAllowed = false
	return writer.enabled
}

func (writer *heartbeatResponseWriter) enable(streaming bool) {
	if !streaming || writer.interval <= 0 {
		return
	}
	writer.lifecycleMu.Lock()
	if writer.enabled || !writer.heartbeatAllowed || writer.stopped {
		writer.lifecycleMu.Unlock()
		return
	}
	writer.enabled = true
	writer.waitGroup.Add(1)
	writer.lifecycleMu.Unlock()

	go writer.runHeartbeat()
}

func (writer *heartbeatResponseWriter) runHeartbeat() {
	defer writer.waitGroup.Done()
	timer := time.NewTimer(writer.interval)
	defer timer.Stop()
	for {
		select {
		case <-writer.context.Done():
			return
		case <-writer.stopChannel:
			return
		case <-writer.activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(writer.interval)
		case <-timer.C:
			if !writer.writeHeartbeat() {
				return
			}
			timer.Reset(writer.interval)
		}
	}
}

func (writer *heartbeatResponseWriter) writeHeartbeat() bool {
	writer.writeMu.Lock()
	defer writer.writeMu.Unlock()
	if writer.context.Err() != nil {
		return false
	}
	if writer.backendHeadersReady && !writer.committed {
		return true
	}
	if writer.committed && !writer.eventStream {
		return false
	}
	if !writer.atEventBoundary {
		return true
	}
	if !writer.lastBackendActivity.IsZero() && time.Since(writer.lastBackendActivity) < writer.interval {
		return true
	}
	if !writer.committed {
		writer.commitHeartbeatLocked()
	}
	if _, err := io.WriteString(writer.writer, streamHeartbeatComment); err != nil {
		writer.cancel()
		return false
	}
	if err := http.NewResponseController(writer.writer).Flush(); err != nil {
		writer.cancel()
		return false
	}
	return true
}

func (writer *heartbeatResponseWriter) writeJSON(status int, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		return
	}

	writer.writeMu.Lock()
	defer writer.writeMu.Unlock()

	if writer.committed && writer.eventStream {
		if !writer.atLineStart {
			if _, err := io.WriteString(writer.writer, "\n\n"); err != nil {
				writer.cancel()
				return
			}
			writer.atLineStart = true
		}
		if _, err := io.WriteString(writer.writer, "event: error\ndata: "); err != nil {
			writer.cancel()
			return
		}
		if _, err := writer.writer.Write(payload); err != nil {
			writer.cancel()
			return
		}
		if _, err := io.WriteString(writer.writer, "\n\n"); err != nil {
			writer.cancel()
			return
		}
		if err := http.NewResponseController(writer.writer).Flush(); err != nil {
			writer.cancel()
		}
		writer.atLineStart = true
		return
	}

	if !writer.committed {
		writer.header.Set("Content-Type", "application/json")
		writer.commitBackendHeadersLocked(status)
	}
	if _, err := writer.writer.Write(append(payload, '\n')); err != nil {
		writer.cancel()
	}
}

func (writer *heartbeatResponseWriter) stop() {
	writer.lifecycleMu.Lock()
	if writer.stopped {
		writer.lifecycleMu.Unlock()
		return
	}
	writer.stopped = true
	if writer.enabled {
		close(writer.stopChannel)
	}
	writer.lifecycleMu.Unlock()
	writer.waitGroup.Wait()

	writer.writeMu.Lock()
	if writer.committed {
		copyTrailers(writer.writer.Header(), writer.header)
	}
	writer.writeMu.Unlock()
}

func copyHeaders(destination, source http.Header) {
	clear(destination)
	for name, values := range source {
		destination[name] = append([]string(nil), values...)
	}
}

func copyTrailers(destination, source http.Header) {
	for _, declaration := range source.Values("Trailer") {
		for name := range strings.SplitSeq(declaration, ",") {
			name = http.CanonicalHeaderKey(strings.TrimSpace(name))
			if name != "" {
				destination[http.TrailerPrefix+name] = append([]string(nil), source.Values(name)...)
			}
		}
	}
	for name, values := range source {
		if strings.HasPrefix(name, http.TrailerPrefix) {
			destination[name] = append([]string(nil), values...)
		}
	}
}
