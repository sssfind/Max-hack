package skillgap

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-pdf/fpdf"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestAnalyzeDOCXAndCache(t *testing.T) {
	t.Parallel()
	docx := makeDOCX(t, []paragraph{
		{style: "Heading1", text: "ПМ.01 Разработка программных модулей"},
		{text: "Студент изучает SQL и проектирование реляционных баз данных."},
	})
	var hits atomic.Int32
	client := clientReturning(func(*http.Request) ([]byte, string) {
		hits.Add(1)
		return docx, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	})
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	analyzer := New(Options{HTTPClient: client, Now: func() time.Time { return now }})
	req := Request{
		Primary: DocumentRef{URL: "https://spolab.firpo.ru/files/program.docx", Type: DocumentPOP},
		Skills: []MarketSkill{
			{Name: "SQL"},
			{Name: "проектирование безопасных баз данных"},
			{Name: "Kubernetes"},
		},
	}

	first, err := analyzer.Analyze(context.Background(), req)
	if err != nil {
		t.Fatalf("first Analyze() error = %v", err)
	}
	if first.Source.Status != RetrievalFetched || first.Source.FromCache {
		t.Fatalf("first source = %+v", first.Source)
	}
	if len(first.Source.SHA256) != 64 || !first.Source.FetchedAt.Equal(now) {
		t.Fatalf("first source metadata = %+v", first.Source)
	}
	if got := first.Matches[0]; got.Status != StatusFound || len(got.Evidence) == 0 || got.Evidence[0].Page != 0 {
		t.Fatalf("SQL match = %+v", got)
	}
	if section := first.Matches[0].Evidence[0].Section; !strings.Contains(section, "ПМ.01") {
		t.Fatalf("evidence section = %q", section)
	}
	if got := first.Matches[1]; got.Status != StatusPartial {
		t.Fatalf("database match = %+v", got)
	}
	if got := first.Matches[2]; got.Status != StatusNotFound || !strings.Contains(got.Message, "не означает") {
		t.Fatalf("not-found match = %+v", got)
	}

	second, err := analyzer.Analyze(context.Background(), req)
	if err != nil {
		t.Fatalf("cached Analyze() error = %v", err)
	}
	if second.Source.Status != RetrievalCache || !second.Source.FromCache || hits.Load() != 1 {
		t.Fatalf("cached source = %+v, HTTP hits = %d", second.Source, hits.Load())
	}
}

func TestAnalyzePOPZipWithNestedDOCX(t *testing.T) {
	t.Parallel()
	docx := makeDOCX(t, []paragraph{{text: "Основы Python и алгоритмизации."}})
	outer := makeZIP(t, map[string][]byte{"programs/09.02.07.docx": docx, "readme.txt": []byte("ignored")})
	analyzer := New(Options{HTTPClient: clientReturning(func(*http.Request) ([]byte, string) {
		return outer, "application/zip"
	})})

	result, err := analyzer.Analyze(context.Background(), Request{
		Primary: DocumentRef{URL: "https://spolab.firpo.ru/pop/archive.zip", Type: DocumentPOP},
		Skills:  []MarketSkill{{Name: "Python"}},
	})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if result.Matches[0].Status != StatusFound || result.Matches[0].Evidence[0].File != "09.02.07.docx" {
		t.Fatalf("result = %+v", result)
	}
}

func TestIdenticalDocumentsShareHashCache(t *testing.T) {
	t.Parallel()
	docx := makeDOCX(t, []paragraph{{text: "SQL"}})
	var hits atomic.Int32
	analyzer := New(Options{HTTPClient: clientReturning(func(*http.Request) ([]byte, string) {
		hits.Add(1)
		return docx, "application/octet-stream"
	})})
	for _, name := range []string{"first.docx", "second.docx"} {
		result, err := analyzer.Analyze(context.Background(), Request{
			Primary: DocumentRef{URL: "https://spolab.firpo.ru/" + name, Type: DocumentPOP},
			Skills:  []MarketSkill{{Name: "SQL"}},
		})
		if err != nil {
			t.Fatalf("Analyze(%q) error = %v", name, err)
		}
		if got := result.Matches[0].Evidence[0].File; got != name {
			t.Fatalf("Analyze(%q) evidence file = %q", name, got)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("HTTP hits = %d, want 2 distinct URL fetches", hits.Load())
	}
	analyzer.mu.Lock()
	hashes := len(analyzer.hashCache)
	analyzer.mu.Unlock()
	if hashes != 1 {
		t.Fatalf("hash cache entries = %d, want 1", hashes)
	}
}

func TestAnalyzeRARFallsBackToFGOS(t *testing.T) {
	t.Parallel()
	rawRAR := append([]byte(nil), magicRAR5...)
	rawRAR = append(rawRAR, []byte("not parsed")...)
	fgos := makeDOCX(t, []paragraph{{text: "Обучение работе с Linux."}})
	client := clientReturning(func(req *http.Request) ([]byte, string) {
		if strings.HasSuffix(req.URL.Path, ".rar") {
			return rawRAR, "application/vnd.rar"
		}
		return fgos, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	})
	analyzer := New(Options{HTTPClient: client})

	result, err := analyzer.Analyze(context.Background(), Request{
		Primary:  DocumentRef{URL: "https://spolab.firpo.ru/pop/program.rar", Type: DocumentPOP},
		Fallback: &DocumentRef{URL: "https://spolab.firpo.ru/fgos/program.docx", Type: DocumentFGOS},
		Skills:   []MarketSkill{{Name: "Linux"}},
	})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if !result.UsedFallback || result.Source.Type != DocumentFGOS || result.Matches[0].Status != StatusFound {
		t.Fatalf("fallback result = %+v", result)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Status != RetrievalUnsupported {
		t.Fatalf("attempts = %+v", result.Attempts)
	}
}

func TestAnalyzePDFProvidesPageEvidence(t *testing.T) {
	t.Parallel()
	payload := makePDF(t, "first page", "Docker and container deployment")
	analyzer := New(Options{HTTPClient: clientReturning(func(*http.Request) ([]byte, string) {
		return payload, "application/pdf"
	})})

	result, err := analyzer.Analyze(context.Background(), Request{
		Primary: DocumentRef{URL: "https://spolab.firpo.ru/fgos/program.pdf", Type: DocumentFGOS},
		Skills:  []MarketSkill{{Name: "Docker"}},
	})
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if got := result.Matches[0]; got.Status != StatusFound || len(got.Evidence) == 0 || got.Evidence[0].Page != 2 {
		t.Fatalf("PDF match = %+v", got)
	}
}

func TestPDFPageLimit(t *testing.T) {
	t.Parallel()
	payload := makePDF(t, "one", "two")
	analyzer := New(Options{
		HTTPClient: clientReturning(func(*http.Request) ([]byte, string) { return payload, "application/pdf" }),
		MaxPages:   1,
	})
	_, err := analyzer.Analyze(context.Background(), Request{
		Primary: DocumentRef{URL: "https://spolab.firpo.ru/fgos/two-pages.pdf", Type: DocumentFGOS},
		Skills:  []MarketSkill{{Name: "one"}},
	})
	if !errors.Is(err, ErrParseLimit) {
		t.Fatalf("Analyze() error = %v, want ErrParseLimit", err)
	}
}

func TestHighlyCompressiblePDFStreamHonorsTextLimit(t *testing.T) {
	t.Parallel()
	payload := makeHighlyCompressiblePDF(t, 8<<20)
	_, err := parsePDF(context.Background(), payload, "compressed.pdf", Options{
		MaxPages:          1,
		MaxTextBytes:      1 << 20,
		PDFParseTimeout:   10 * time.Second,
		PDFMaxMemoryBytes: 512 << 20,
	})
	if !errors.Is(err, ErrParseLimit) {
		t.Fatalf("parsePDF() error = %v, want ErrParseLimit", err)
	}
}

func TestPDFDecompressionBombIsContainedByWorker(t *testing.T) {
	if runtime.GOOS != "linux" || pdfRaceEnabled {
		t.Skip("hard RLIMIT_AS regression applies to non-race Linux builds")
	}

	options := Options{
		MaxPages:          1,
		MaxTextBytes:      1 << 20,
		PDFParseTimeout:   15 * time.Second,
		PDFMaxMemoryBytes: minPDFWorkerMemory,
	}
	// Prove the initialized worker and the minimum supported parser budget can
	// handle a normal document; otherwise the bomb assertion is a false positive.
	if _, err := parsePDF(context.Background(), makePDF(t, "small PDF"), "small.pdf", options); err != nil {
		t.Fatalf("bounded worker could not parse a normal PDF: %v", err)
	}

	// The compressed stream is only a few megabytes, but its single PDF string
	// token expands well beyond the worker's parser budget. Older in-process
	// parsing allowed this allocation to threaten the whole bot process.
	payload := makeHighlyCompressiblePDF(t, options.PDFMaxMemoryBytes+(512<<20))
	started := time.Now()
	_, err := parsePDF(context.Background(), payload, "bomb.pdf", options)
	if !errors.Is(err, ErrParseLimit) {
		t.Fatalf("parsePDF() error = %v, want ErrParseLimit", err)
	}
	if !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("parsePDF() error = %v, want a hard worker termination", err)
	}
	if elapsed := time.Since(started); elapsed > options.PDFParseTimeout+3*time.Second {
		t.Fatalf("isolated parser returned after %s, want a hard bounded duration", elapsed)
	}
}

func TestSourceSafetyAndLimits(t *testing.T) {
	t.Parallel()
	docx := makeDOCX(t, []paragraph{{text: "SQL"}})
	tests := []struct {
		name    string
		url     string
		options Options
		wantErr error
	}{
		{name: "HTTP", url: "http://spolab.firpo.ru/a.docx", wantErr: ErrUnsafeSource},
		{name: "foreign host", url: "https://example.com/a.docx", wantErr: ErrUnsafeSource},
		{name: "credentials", url: "https://user@spolab.firpo.ru/a.docx", wantErr: ErrUnsafeSource},
		{
			name: "download limit", url: "https://spolab.firpo.ru/a.docx", wantErr: ErrDownloadTooLarge,
			options: Options{MaxDownloadBytes: 4, HTTPClient: clientReturning(func(*http.Request) ([]byte, string) {
				return docx, "application/octet-stream"
			})},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.options.HTTPClient == nil {
				test.options.HTTPClient = clientReturning(func(*http.Request) ([]byte, string) { return docx, "application/octet-stream" })
			}
			analyzer := New(test.options)
			_, err := analyzer.Analyze(context.Background(), Request{
				Primary: DocumentRef{URL: test.url, Type: DocumentPOP},
				Skills:  []MarketSkill{{Name: "SQL"}},
			})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Analyze() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestRedirectIsRevalidated(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == AllowedHost {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"https://evil.example/document.pdf"}},
				Body:       io.NopCloser(strings.NewReader("redirect")),
				Request:    req,
			}, nil
		}
		return nil, errors.New("foreign host must never be requested")
	})}
	analyzer := New(Options{HTTPClient: client})
	_, err := analyzer.Analyze(context.Background(), Request{
		Primary: DocumentRef{URL: "https://spolab.firpo.ru/redirect", Type: DocumentPOP},
		Skills:  []MarketSkill{{Name: "SQL"}},
	})
	if !errors.Is(err, ErrUnsafeSource) {
		t.Fatalf("Analyze() error = %v, want ErrUnsafeSource", err)
	}
}

func TestDOCXExpandedTextLimit(t *testing.T) {
	t.Parallel()
	docx := makeDOCX(t, []paragraph{{text: strings.Repeat("skill ", 100)}})
	analyzer := New(Options{
		HTTPClient:   clientReturning(func(*http.Request) ([]byte, string) { return docx, "application/octet-stream" }),
		MaxTextBytes: 100,
	})
	_, err := analyzer.Analyze(context.Background(), Request{
		Primary: DocumentRef{URL: "https://spolab.firpo.ru/large.docx", Type: DocumentPOP},
		Skills:  []MarketSkill{{Name: "skill"}},
	})
	if !errors.Is(err, ErrParseLimit) {
		t.Fatalf("Analyze() error = %v, want ErrParseLimit", err)
	}
}

func TestNestedDOCXArchiveUsesAggregateXMLBudget(t *testing.T) {
	t.Parallel()
	docx := makeDOCX(t, []paragraph{{text: strings.Repeat("учебный текст ", 20)}})
	reader, err := zip.NewReader(bytes.NewReader(docx), int64(len(docx)))
	if err != nil {
		t.Fatal(err)
	}
	documentXML := findZipEntry(reader.File, "word/document.xml")
	if documentXML == nil {
		t.Fatal("test DOCX has no document.xml")
	}
	perDocument := int(documentXML.UncompressedSize64)
	outer := makeZIP(t, map[string][]byte{"first.docx": docx, "second.docx": docx})
	analyzer := New(Options{
		HTTPClient:  clientReturning(func(*http.Request) ([]byte, string) { return outer, "application/zip" }),
		MaxXMLBytes: perDocument + 1,
	})
	_, err = analyzer.Analyze(context.Background(), Request{
		Primary: DocumentRef{URL: "https://spolab.firpo.ru/multi.zip", Type: DocumentPOP},
		Skills:  []MarketSkill{{Name: "учебный"}},
	})
	if !errors.Is(err, ErrParseLimit) {
		t.Fatalf("Analyze() error = %v, want aggregate ErrParseLimit", err)
	}
}

func TestParsedDocumentCacheHonorsByteBudget(t *testing.T) {
	t.Parallel()
	docx := makeDOCX(t, []paragraph{{text: strings.Repeat("SQL и базы данных ", 20)}})
	analyzer := New(Options{
		HTTPClient:    clientReturning(func(*http.Request) ([]byte, string) { return docx, "application/octet-stream" }),
		MaxCacheBytes: 128,
	})
	_, err := analyzer.Analyze(context.Background(), Request{
		Primary: DocumentRef{URL: "https://spolab.firpo.ru/cache-limit.docx", Type: DocumentPOP},
		Skills:  []MarketSkill{{Name: "SQL"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	analyzer.mu.Lock()
	cachedBytes := cacheSize(analyzer.urlCache) + cacheSize(analyzer.hashCache)
	analyzer.mu.Unlock()
	if cachedBytes > 128 {
		t.Fatalf("cache size = %d, want <= 128", cachedBytes)
	}
}

func TestLongFragmentsAndHeadingsAreBounded(t *testing.T) {
	t.Parallel()
	parts := splitFragments(strings.Repeat("я", 12_345))
	if len(parts) < 10 {
		t.Fatalf("fragment count = %d, want multiple bounded chunks", len(parts))
	}
	for _, part := range parts {
		if got := utf8.RuneCountInString(part); got > 1200 {
			t.Fatalf("fragment length = %d, want <= 1200", got)
		}
	}
	if got := utf8.RuneCountInString(boundedMetadata(strings.Repeat("з", 10_000), 240)); got != 240 {
		t.Fatalf("bounded heading length = %d, want 240", got)
	}
}

func TestAnalyzeRejectsDuplicateAndExcessSkills(t *testing.T) {
	t.Parallel()
	analyzer := New(Options{MaxSkills: 1})
	request := Request{
		Primary: DocumentRef{URL: "https://spolab.firpo.ru/a.pdf", Type: DocumentFGOS},
		Skills:  []MarketSkill{{Name: "SQL"}, {Name: "sql"}},
	}
	_, err := analyzer.Analyze(context.Background(), request)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Analyze() error = %v, want ErrInvalidRequest", err)
	}
}

func TestUnsafePrimaryDoesNotFallBack(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	analyzer := New(Options{HTTPClient: clientReturning(func(*http.Request) ([]byte, string) {
		hits.Add(1)
		return makeDOCX(t, []paragraph{{text: "SQL"}}), "application/octet-stream"
	})})
	_, err := analyzer.Analyze(context.Background(), Request{
		Primary:  DocumentRef{URL: "https://evil.example/program.docx", Type: DocumentPOP},
		Fallback: &DocumentRef{URL: "https://spolab.firpo.ru/fgos/program.docx", Type: DocumentFGOS},
		Skills:   []MarketSkill{{Name: "SQL"}},
	})
	if !errors.Is(err, ErrUnsafeSource) || hits.Load() != 0 {
		t.Fatalf("Analyze() error = %v, hits = %d", err, hits.Load())
	}
}

func clientReturning(payload func(*http.Request) ([]byte, string)) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		data, contentType := payload(req)
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{contentType}},
			Body:          io.NopCloser(bytes.NewReader(data)),
			ContentLength: int64(len(data)),
			Request:       req,
		}, nil
	})}
}

type paragraph struct {
	style string
	text  string
}

func makeDOCX(t *testing.T, paragraphs []paragraph) []byte {
	t.Helper()
	var xmlBody strings.Builder
	xmlBody.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for _, item := range paragraphs {
		xmlBody.WriteString("<w:p>")
		if item.style != "" {
			xmlBody.WriteString(`<w:pPr><w:pStyle w:val="` + xmlEscape(item.style) + `"/></w:pPr>`)
		}
		xmlBody.WriteString(`<w:r><w:t>` + xmlEscape(item.text) + `</w:t></w:r></w:p>`)
	}
	xmlBody.WriteString("</w:body></w:document>")
	return makeZIP(t, map[string][]byte{
		"[Content_Types].xml": []byte(`<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"></Types>`),
		"word/document.xml":   []byte(xmlBody.String()),
	})
}

func makeZIP(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, data := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("Create(%q): %v", name, err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatalf("Write(%q): %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip Close(): %v", err)
	}
	return buffer.Bytes()
}

func makePDF(t *testing.T, pages ...string) []byte {
	t.Helper()
	document := fpdf.New("P", "mm", "A4", "")
	document.SetCompression(false)
	for _, text := range pages {
		document.AddPage()
		document.SetFont("Arial", "", 12)
		document.Cell(40, 10, text)
	}
	var buffer bytes.Buffer
	if err := document.Output(&buffer); err != nil {
		t.Fatalf("PDF Output(): %v", err)
	}
	return buffer.Bytes()
}

func makeHighlyCompressiblePDF(t *testing.T, expandedBytes int64) []byte {
	t.Helper()
	var compressed bytes.Buffer
	compressor, err := zlib.NewWriterLevel(&compressed, zlib.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(compressor, "BT ("); err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte{'A'}, 1<<20)
	for remaining := expandedBytes; remaining > 0; {
		part := int64(len(chunk))
		if part > remaining {
			part = remaining
		}
		if _, err := compressor.Write(chunk[:part]); err != nil {
			t.Fatal(err)
		}
		remaining -= part
	}
	if _, err := io.WriteString(compressor, ") Tj ET"); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}

	var document bytes.Buffer
	document.WriteString("%PDF-1.4\n")
	offsets := make([]int, 5)
	writeObject := func(number int, body string) {
		offsets[number] = document.Len()
		fmt.Fprintf(&document, "%d 0 obj\n%s\nendobj\n", number, body)
	}
	writeObject(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObject(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObject(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << >> /Contents 4 0 R >>")
	offsets[4] = document.Len()
	fmt.Fprintf(&document, "4 0 obj\n<< /Length %d /Filter /FlateDecode >>\nstream\n", compressed.Len())
	document.Write(compressed.Bytes())
	document.WriteString("\nendstream\nendobj\n")
	xrefOffset := document.Len()
	document.WriteString("xref\n0 5\n0000000000 65535 f \n")
	for number := 1; number <= 4; number++ {
		fmt.Fprintf(&document, "%010d 00000 n \n", offsets[number])
	}
	fmt.Fprintf(&document, "trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xrefOffset)
	return document.Bytes()
}

func xmlEscape(value string) string {
	var buffer bytes.Buffer
	if err := xml.EscapeText(&buffer, []byte(value)); err != nil {
		panic(err)
	}
	return buffer.String()
}
