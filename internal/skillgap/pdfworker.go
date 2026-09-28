package skillgap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	pdfWorkerModeEnv   = "SKILLGAP_INTERNAL_PDF_WORKER"
	pdfWorkerModeValue = "1"

	minPDFWorkerMemory = int64(128 << 20)
	maxPDFWorkerMemory = int64(2 << 30)
	maxPDFWorkerInput  = int64(128 << 20)
	maxPDFWorkerText   = 64 << 20
	maxPDFWorkerPages  = 5_000
	maxPDFWorkerTime   = 2 * time.Minute
	maxWorkerHeader    = 16 << 10
	maxWorkerError     = 2 << 10
	maxWorkerOutput    = int64(512 << 20)
)

type pdfWorkerRequest struct {
	PayloadBytes int64 `json:"payload_bytes"`
	MaxTextBytes int   `json:"max_text_bytes"`
	MaxPages     int   `json:"max_pages"`
	TimeoutMS    int64 `json:"timeout_ms"`
}

type pdfWorkerFragment struct {
	Text string `json:"text"`
	Page int    `json:"page"`
}

type pdfWorkerResponse struct {
	Fragments []pdfWorkerFragment `json:"fragments,omitempty"`
	ErrorKind string              `json:"error_kind,omitempty"`
	Error     string              `json:"error,omitempty"`
}

// A per-process limit protects one parser worker. Serializing workers also
// bounds aggregate memory when several users submit hostile PDFs at once.
var pdfWorkerSlot = make(chan struct{}, 1)

// The PDF parser is deliberately run before the host program's main function
// when this private marker is present. That makes isolation self-contained for
// every binary importing this package, including the production bot, without
// requiring a second executable in the image.
func init() {
	if os.Getenv(pdfWorkerModeEnv) != pdfWorkerModeValue {
		return
	}

	// Some PDF filters in the parser dependency print diagnostics to stdout.
	// Preserve the original descriptor for the protocol and redirect such
	// diagnostics away from it so they cannot corrupt the response.
	protocolOutput := os.Stdout
	os.Stdout = os.Stderr
	os.Exit(servePDFWorker(os.Stdin, protocolOutput))
}

func parsePDFIsolated(ctx context.Context, payload []byte, fileName string, opts Options) ([]fragment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case pdfWorkerSlot <- struct{}{}:
		defer func() { <-pdfWorkerSlot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	parseTimeout := opts.PDFParseTimeout
	if parseTimeout <= 0 {
		parseTimeout = defaultPDFParseTimeout
	}
	memoryLimit := opts.PDFMaxMemoryBytes
	if memoryLimit <= 0 {
		memoryLimit = defaultPDFMaxMemoryBytes
	}
	if err := validatePDFWorkerLimits(int64(len(payload)), opts.MaxTextBytes, opts.MaxPages, parseTimeout, memoryLimit); err != nil {
		return nil, err
	}

	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate isolated PDF parser: %w", err)
	}
	request := pdfWorkerRequest{
		PayloadBytes: int64(len(payload)),
		MaxTextBytes: opts.MaxTextBytes,
		MaxPages:     opts.MaxPages,
		TimeoutMS:    parseTimeout.Milliseconds(),
	}
	header, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode isolated PDF parser request: %w", err)
	}
	header = append(header, '\n')

	workerCtx, cancel := context.WithTimeout(ctx, parseTimeout)
	defer cancel()
	command := newPDFWorkerCommand(workerCtx, executable, memoryLimit, parseTimeout)
	command.Env = pdfWorkerEnvironment(memoryLimit)
	command.Stdin = io.MultiReader(bytes.NewReader(header), bytes.NewReader(payload))
	stdout := newCappedBuffer(pdfWorkerOutputLimit(opts.MaxTextBytes, opts.MaxPages))
	stderr := newCappedBuffer(maxWorkerError)
	command.Stdout = stdout
	command.Stderr = stderr

	runErr := command.Run()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errors.Is(workerCtx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w: isolated PDF parser exceeded %s", ErrParseLimit, parseTimeout)
	}
	if stdout.overflowed || stderr.overflowed {
		return nil, fmt.Errorf("%w: isolated PDF parser exceeded its output limit", ErrParseLimit)
	}
	if runErr != nil {
		// A hard address-space or CPU limit terminates the worker without a
		// protocol response. Treat every abnormal worker exit as a parse limit;
		// the untrusted document must never take down the bot process.
		return nil, fmt.Errorf("%w: isolated PDF parser terminated", ErrParseLimit)
	}

	var response pdfWorkerResponse
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("%w: invalid isolated PDF parser response", ErrParseLimit)
	}
	if response.ErrorKind != "" {
		return nil, pdfWorkerResponseError(response)
	}
	if len(response.Fragments) == 0 {
		return nil, ErrNoExtractableText
	}
	fragments := make([]fragment, len(response.Fragments))
	for index, item := range response.Fragments {
		fragments[index] = fragment{text: item.Text, page: item.Page, file: fileName}
	}
	return fragments, nil
}

func newPDFWorkerCommand(ctx context.Context, executable string, memoryLimit int64, timeout time.Duration) *exec.Cmd {
	if runtime.GOOS != "linux" || pdfRaceEnabled {
		return exec.CommandContext(ctx, executable)
	}

	// Apply limits in the shell before exec starts the Go runtime. Applying an
	// address-space limit from Go itself is too late because the runtime may
	// already have reserved a large arena. /bin/sh is present in the production
	// Alpine image and on the Linux CI runners.
	memoryKB := strconv.FormatInt(memoryLimit/1024, 10)
	cpuSeconds := strconv.FormatInt(maxInt64(1, (timeout.Milliseconds()+999)/1000), 10)
	const script = `ulimit -v "$1" && ulimit -t "$2" && exec "$3"`
	return exec.CommandContext(ctx, "/bin/sh", "-c", script, "skillgap-pdf-worker", memoryKB, cpuSeconds, executable)
}

func pdfWorkerEnvironment(memoryLimit int64) []string {
	// Do not pass bot/database credentials to the parser process. Windows needs
	// SystemRoot to initialize parts of the standard library, while the Linux
	// worker needs no inherited environment at all.
	environment := []string{
		pdfWorkerModeEnv + "=" + pdfWorkerModeValue,
		"GOMEMLIMIT=" + strconv.FormatInt(memoryLimit*3/5, 10) + "B",
		"GOGC=50",
		"GOTRACEBACK=none",
	}
	for _, name := range []string{"SystemRoot", "WINDIR", "TEMP", "TMP"} {
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

func servePDFWorker(input io.Reader, output io.Writer) int {
	reader := bufio.NewReaderSize(input, maxWorkerHeader)
	header, err := reader.ReadSlice('\n')
	if err != nil {
		return writePDFWorkerResponse(output, pdfWorkerResponse{ErrorKind: "request", Error: "invalid worker request"})
	}
	var request pdfWorkerRequest
	if err := json.Unmarshal(bytes.TrimSpace(header), &request); err != nil {
		return writePDFWorkerResponse(output, pdfWorkerResponse{ErrorKind: "request", Error: "invalid worker request"})
	}
	timeout := time.Duration(request.TimeoutMS) * time.Millisecond
	if err := validatePDFWorkerLimits(request.PayloadBytes, request.MaxTextBytes, request.MaxPages, timeout, defaultPDFMaxMemoryBytes); err != nil {
		return writePDFWorkerResponse(output, pdfWorkerResponse{ErrorKind: "parse_limit", Error: err.Error()})
	}
	payload := make([]byte, request.PayloadBytes)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return writePDFWorkerResponse(output, pdfWorkerResponse{ErrorKind: "request", Error: "truncated worker request"})
	}

	workerCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	fragments, err := parsePDFInProcess(workerCtx, payload, "", Options{
		MaxTextBytes: request.MaxTextBytes,
		MaxPages:     request.MaxPages,
	})
	if err != nil {
		kind := "parse"
		switch {
		case errors.Is(err, ErrParseLimit), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			kind = "parse_limit"
		case errors.Is(err, ErrNoExtractableText):
			kind = "no_text"
		}
		return writePDFWorkerResponse(output, pdfWorkerResponse{ErrorKind: kind, Error: boundedPDFWorkerError(err)})
	}
	response := pdfWorkerResponse{Fragments: make([]pdfWorkerFragment, len(fragments))}
	for index, item := range fragments {
		response.Fragments[index] = pdfWorkerFragment{Text: item.text, Page: item.page}
	}
	return writePDFWorkerResponse(output, response)
}

func writePDFWorkerResponse(output io.Writer, response pdfWorkerResponse) int {
	if err := json.NewEncoder(output).Encode(response); err != nil {
		return 70
	}
	return 0
}

func pdfWorkerResponseError(response pdfWorkerResponse) error {
	message := strings.TrimSpace(response.Error)
	if message == "" {
		message = "isolated PDF parser rejected the document"
	}
	switch response.ErrorKind {
	case "parse_limit", "request":
		return fmt.Errorf("%w: %s", ErrParseLimit, message)
	case "no_text":
		return fmt.Errorf("%w: %s", ErrNoExtractableText, message)
	default:
		return errors.New(message)
	}
}

func validatePDFWorkerLimits(payloadBytes int64, maxTextBytes, maxPages int, timeout time.Duration, memoryLimit int64) error {
	switch {
	case payloadBytes <= 0 || payloadBytes > maxPDFWorkerInput:
		return fmt.Errorf("%w: PDF input size is outside the worker limit", ErrParseLimit)
	case maxTextBytes <= 0 || maxTextBytes > maxPDFWorkerText:
		return fmt.Errorf("%w: PDF text limit is outside the worker limit", ErrParseLimit)
	case maxPages <= 0 || maxPages > maxPDFWorkerPages:
		return fmt.Errorf("%w: PDF page limit is outside the worker limit", ErrParseLimit)
	case timeout <= 0 || timeout > maxPDFWorkerTime:
		return fmt.Errorf("%w: PDF time limit is outside the worker limit", ErrParseLimit)
	case memoryLimit < minPDFWorkerMemory || memoryLimit > maxPDFWorkerMemory:
		return fmt.Errorf("%w: PDF memory limit is outside the worker limit", ErrParseLimit)
	default:
		return nil
	}
}

func pdfWorkerOutputLimit(maxTextBytes, maxPages int) int64 {
	limit := int64(maxTextBytes)*8 + int64(maxPages)*64 + (1 << 20)
	if limit > maxWorkerOutput {
		return maxWorkerOutput
	}
	return limit
}

func boundedPDFWorkerError(err error) string {
	message := err.Error()
	if len(message) <= maxWorkerError {
		return message
	}
	return message[:maxWorkerError]
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

type cappedBuffer struct {
	buffer     bytes.Buffer
	remaining  int64
	overflowed bool
}

func newCappedBuffer(limit int64) *cappedBuffer {
	return &cappedBuffer{remaining: limit}
}

func (b *cappedBuffer) Write(payload []byte) (int, error) {
	originalLength := len(payload)
	if int64(len(payload)) > b.remaining {
		payload = payload[:maxInt64(0, b.remaining)]
		b.overflowed = true
	}
	if len(payload) > 0 {
		_, _ = b.buffer.Write(payload)
		b.remaining -= int64(len(payload))
	}
	return originalLength, nil
}

func (b *cappedBuffer) Bytes() []byte {
	return b.buffer.Bytes()
}
