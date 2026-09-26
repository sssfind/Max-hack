package maxapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

type failingRoundTripper struct {
	requests atomic.Int32
}

func (r *failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.requests.Add(1)
	return nil, errors.New("response lost")
}

func TestSendFileUploadsMultipartAndRetriesAttachmentNotReady(t *testing.T) {
	t.Parallel()

	const (
		chatID    = int64(4242)
		apiToken  = "bot-secret"
		fileToken = "opaque-file-token"
	)
	fileData := []byte("%PDF-1.7\nsmall report")
	var uploadURLRequests atomic.Int32
	var multipartRequests atomic.Int32
	var messageRequests atomic.Int32

	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/uploads":
			uploadURLRequests.Add(1)
			if r.Method != http.MethodPost {
				t.Errorf("upload URL method = %s, want POST", r.Method)
			}
			if got := r.URL.Query().Get("type"); got != "file" {
				t.Errorf("upload type = %q, want file", got)
			}
			if got := r.Header.Get("Authorization"); got != apiToken {
				t.Errorf("upload URL authorization = %q, want %q", got, apiToken)
			}
			requestBody, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read upload URL request: %v", err)
			}
			if len(requestBody) != 0 {
				t.Errorf("upload URL request body length = %d, want 0", len(requestBody))
			}
			_ = json.NewEncoder(w).Encode(UploadEndpoint{URL: server.URL + "/binary-upload?sig=abc123&expires=42"})

		case "/binary-upload":
			multipartRequests.Add(1)
			if r.Method != http.MethodPost {
				t.Errorf("multipart method = %s, want POST", r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("multipart request leaked Authorization header %q", got)
			}
			if got := r.URL.RawQuery; got != "sig=abc123&expires=42" {
				t.Errorf("multipart upload query = %q, want signed query unchanged", got)
			}
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data; boundary=") {
				t.Errorf("multipart content type = %q", r.Header.Get("Content-Type"))
			}
			if r.ContentLength <= int64(len(fileData)) {
				t.Errorf("multipart content length = %d, want greater than file size %d", r.ContentLength, len(fileData))
			}

			reader, err := r.MultipartReader()
			if err != nil {
				t.Errorf("create multipart reader: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			part, err := reader.NextPart()
			if err != nil {
				t.Errorf("read multipart part: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if got := part.FormName(); got != "data" {
				t.Errorf("multipart form name = %q, want data", got)
			}
			if got := part.FileName(); got != "skillgap.pdf" {
				t.Errorf("multipart filename = %q, want skillgap.pdf", got)
			}
			gotData, err := io.ReadAll(part)
			if err != nil {
				t.Errorf("read multipart data: %v", err)
			}
			if string(gotData) != string(fileData) {
				t.Errorf("multipart data = %q, want %q", gotData, fileData)
			}
			if extra, err := reader.NextPart(); err != io.EOF || extra != nil {
				t.Errorf("unexpected extra multipart part: part=%v err=%v", extra, err)
			}
			_ = json.NewEncoder(w).Encode(FileUploadResult{Token: fileToken})

		case "/messages":
			attempt := messageRequests.Add(1)
			if r.Method != http.MethodPost {
				t.Errorf("message method = %s, want POST", r.Method)
			}
			if got := r.Header.Get("Authorization"); got != apiToken {
				t.Errorf("message authorization = %q, want %q", got, apiToken)
			}
			if got := r.URL.Query().Get("chat_id"); got != "4242" {
				t.Errorf("message chat_id = %q, want 4242", got)
			}
			var body struct {
				Text        string `json:"text"`
				Attachments []struct {
					Type    string `json:"type"`
					Payload struct {
						Token string `json:"token"`
					} `json:"payload"`
				} `json:"attachments"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode message body: %v", err)
			}
			if body.Text != "Отчёт готов" {
				t.Errorf("message text = %q", body.Text)
			}
			if len(body.Attachments) != 1 || body.Attachments[0].Type != "file" || body.Attachments[0].Payload.Token != fileToken {
				t.Errorf("unexpected message attachment: %+v", body.Attachments)
			}

			if attempt == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"code":"attachment.not.ready","message":"still processing"}`)
				return
			}
			_, _ = io.WriteString(w, `{}`)

		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newClient(apiToken, server.URL, server.Client())
	client.global = rate.NewLimiter(rate.Inf, 1)
	client.limiters.Store(chatID, rate.NewLimiter(rate.Inf, 1))
	var waits []time.Duration
	client.waitFn = func(ctx context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}

	err := client.SendFile(context.Background(), chatID, 0, `reports\skillgap.pdf`, fileData, "Отчёт готов")
	if err != nil {
		t.Fatalf("SendFile() error = %v", err)
	}
	if got := uploadURLRequests.Load(); got != 1 {
		t.Errorf("upload URL requests = %d, want 1", got)
	}
	if got := multipartRequests.Load(); got != 1 {
		t.Errorf("multipart requests = %d, want 1", got)
	}
	if got := messageRequests.Load(); got != 2 {
		t.Errorf("message requests = %d, want 2", got)
	}
	if len(waits) != 1 || waits[0] != attachmentReadyInitialBackoff {
		t.Errorf("retry waits = %v, want [%v]", waits, attachmentReadyInitialBackoff)
	}
}

func TestSendFileDoesNotRetryOtherAPIErrors(t *testing.T) {
	t.Parallel()

	const chatID = int64(17)
	var messageRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/uploads":
			_ = json.NewEncoder(w).Encode(UploadEndpoint{URL: server.URL + "/binary-upload"})
		case "/binary-upload":
			_ = json.NewEncoder(w).Encode(FileUploadResult{Token: "token"})
		case "/messages":
			messageRequests.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":"message.invalid","message":"bad message"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newClient("token", server.URL, server.Client())
	client.global = rate.NewLimiter(rate.Inf, 1)
	client.limiters.Store(chatID, rate.NewLimiter(rate.Inf, 1))
	var waitCalls atomic.Int32
	client.waitFn = func(context.Context, time.Duration) error {
		waitCalls.Add(1)
		return nil
	}

	err := client.SendFile(context.Background(), chatID, 0, "report.pdf", []byte("pdf"), "")
	if err == nil || !strings.Contains(err.Error(), "message.invalid") {
		t.Fatalf("SendFile() error = %v, want message.invalid", err)
	}
	if got := messageRequests.Load(); got != 1 {
		t.Errorf("message requests = %d, want 1", got)
	}
	if got := waitCalls.Load(); got != 0 {
		t.Errorf("wait calls = %d, want 0", got)
	}
}

func TestSendMessageDoesNotRetryAmbiguousNetworkFailure(t *testing.T) {
	t.Parallel()

	transport := &failingRoundTripper{}
	client := newClient("token", "https://api.example", &http.Client{Transport: transport})
	client.global = rate.NewLimiter(rate.Inf, 1)
	client.limiters.Store(int64(91), rate.NewLimiter(rate.Inf, 1))

	err := client.SendMessage(context.Background(), 91, 0, NewMessageBody{Text: Ptr("Отчёт")})
	if err == nil || !strings.Contains(err.Error(), "response lost") {
		t.Fatalf("SendMessage() error = %v, want response-lost error", err)
	}
	if got := transport.requests.Load(); got != 1 {
		t.Fatalf("message requests = %d, want exactly 1", got)
	}
}

func TestUploadFileRejectsNonHTTPSUploadURL(t *testing.T) {
	t.Parallel()

	var binaryRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/uploads":
			_ = json.NewEncoder(w).Encode(UploadEndpoint{URL: server.URL + "/binary-upload"})
		case "/binary-upload":
			binaryRequests.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newClient("token", server.URL, server.Client())
	client.global = rate.NewLimiter(rate.Inf, 1)
	_, err := client.UploadFile(context.Background(), "report.pdf", []byte("pdf"))
	if err == nil || !strings.Contains(err.Error(), "absolute HTTPS") {
		t.Fatalf("UploadFile() error = %v, want HTTPS validation error", err)
	}
	if got := binaryRequests.Load(); got != 0 {
		t.Errorf("binary upload requests = %d, want 0", got)
	}
}

func TestUploadFileRejectsRedirectDowngradeBeforeSendingBody(t *testing.T) {
	t.Parallel()

	var insecureRequests atomic.Int32
	insecure := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		insecureRequests.Add(1)
	}))
	defer insecure.Close()

	var secure *httptest.Server
	secure = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/uploads":
			_ = json.NewEncoder(w).Encode(UploadEndpoint{URL: secure.URL + "/binary-upload"})
		case "/binary-upload":
			w.Header().Set("Location", insecure.URL+"/stolen")
			w.WriteHeader(http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, r)
		}
	}))
	defer secure.Close()

	client := newClient("token", secure.URL, secure.Client())
	client.global = rate.NewLimiter(rate.Inf, 1)
	_, err := client.UploadFile(context.Background(), "report.pdf", []byte("secret-pdf"))
	if err == nil || !strings.Contains(err.Error(), "absolute HTTPS") {
		t.Fatalf("UploadFile() error = %v, want redirect HTTPS validation error", err)
	}
	if got := insecureRequests.Load(); got != 0 {
		t.Fatalf("redirect target received %d requests", got)
	}
}

func TestValidateUploadURLRejectsPrivateHostsInProduction(t *testing.T) {
	t.Parallel()

	for _, rawURL := range []string{
		"https://127.0.0.1/upload",
		"https://[::1]/upload",
		"https://10.0.0.5/upload",
		"https://100.64.0.1/upload",
		"https://metadata.local/upload",
		"https://198.18.0.1/upload",
		"https://localhost./upload",
		"https://[fec0::1]/upload",
		"https://[100:0:0:1::1]/upload",
	} {
		if err := validateUploadURL(rawURL, false); err == nil {
			t.Errorf("validateUploadURL(%q) accepted private host", rawURL)
		}
	}
	for _, rawURL := range []string{
		"https://fu.oneme.ru/api/upload.do",
		"https://omu.okcdn.ru/upload.do",
		"https://uploads.max.ru/file",
	} {
		if err := validateUploadURL(rawURL, false); err != nil {
			t.Errorf("documented MAX upload URL %q rejected: %v", rawURL, err)
		}
	}
	for _, rawURL := range []string{
		"https://upload.example.com/upload",
		"https://fu.oneme.ru.attacker.example/upload",
	} {
		if err := validateUploadURL(rawURL, false); err == nil {
			t.Errorf("untrusted upload URL %q accepted", rawURL)
		}
	}
}

func TestGuardedUploadTransportRejectsHostnameResolvingToPrivateIP(t *testing.T) {
	t.Parallel()

	var dialCalls atomic.Int32
	base := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialCalls.Add(1)
			return nil, errors.New("must not dial")
		},
	}
	transport, err := guardedUploadTransport(base, func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = transport.DialContext(context.Background(), "tcp", "upload.example:443")
	if err == nil || !strings.Contains(err.Error(), "private or local") {
		t.Fatalf("guarded dial error = %v, want private-address rejection", err)
	}
	if got := dialCalls.Load(); got != 0 {
		t.Fatalf("underlying dial calls = %d, want 0", got)
	}
}

func TestUploadFileRejectsOversizeBeforeNetwork(t *testing.T) {
	t.Parallel()

	client := newClient("token", "https://example.invalid", nil)
	_, err := client.UploadFile(context.Background(), "report.pdf", make([]byte, maxFileUploadSize+1))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("UploadFile() error = %v, want size limit error", err)
	}
}

func TestFileAttachment(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(FileAttachment("file-token"))
	if err != nil {
		t.Fatalf("marshal FileAttachment: %v", err)
	}
	const want = `{"type":"file","payload":{"token":"file-token"}}`
	if string(encoded) != want {
		t.Errorf("FileAttachment JSON = %s, want %s", encoded, want)
	}
}
