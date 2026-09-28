package skillgap

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	pdfreader "github.com/dslipak/pdf"
)

var (
	magicPDF  = []byte("%PDF-")
	magicRAR4 = []byte{'R', 'a', 'r', '!', 0x1a, 0x07, 0x00}
	magicRAR5 = []byte{'R', 'a', 'r', '!', 0x1a, 0x07, 0x01, 0x00}
)

func parseDocument(ctx context.Context, payload []byte, mediaType, fileName string, opts Options) (parsedDocument, error) {
	if err := ctx.Err(); err != nil {
		return parsedDocument{}, err
	}
	mediaType = effectiveMediaType(payload, mediaType)
	switch {
	case bytes.HasPrefix(payload, magicPDF):
		fragments, err := parsePDF(ctx, payload, fileName, opts)
		if err != nil {
			return parsedDocument{}, err
		}
		return parsedDocument{fragments: fragments, mediaType: "application/pdf"}, nil
	case bytes.HasPrefix(payload, magicRAR4), bytes.HasPrefix(payload, magicRAR5):
		return parsedDocument{}, fmt.Errorf("%w: RAR archives are not parsed; use the FGOS fallback", ErrUnsupportedFormat)
	case isZIP(payload):
		fragments, detectedType, err := parseZIP(ctx, payload, fileName, opts)
		if err != nil {
			return parsedDocument{}, err
		}
		return parsedDocument{fragments: fragments, mediaType: detectedType}, nil
	default:
		return parsedDocument{}, fmt.Errorf("%w: media type %q", ErrUnsupportedFormat, mediaType)
	}
}

func effectiveMediaType(payload []byte, declared string) string {
	detected := strings.ToLower(http.DetectContentType(firstBytes(payload, 512)))
	if detected != "application/octet-stream" {
		return detected
	}
	return strings.ToLower(declared)
}

func firstBytes(data []byte, n int) []byte {
	if len(data) < n {
		return data
	}
	return data[:n]
}

func isZIP(payload []byte) bool {
	return len(payload) >= 4 && payload[0] == 'P' && payload[1] == 'K' &&
		((payload[2] == 3 && payload[3] == 4) || (payload[2] == 5 && payload[3] == 6) || (payload[2] == 7 && payload[3] == 8))
}

func parsePDF(ctx context.Context, payload []byte, fileName string, opts Options) (fragments []fragment, err error) {
	return parsePDFIsolated(ctx, payload, fileName, opts)
}

// parsePDFInProcess runs only inside the resource-constrained PDF worker.
// Keeping the third-party parser out of the long-lived bot process is
// important: PDF stream filters can expand before GetPlainText returns, so an
// output-size check in this function alone cannot stop a decompression bomb.
func parsePDFInProcess(ctx context.Context, payload []byte, fileName string, opts Options) (fragments []fragment, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			fragments = nil
			err = fmt.Errorf("parse PDF safely: %v", recovered)
		}
	}()

	reader, err := pdfreader.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return nil, fmt.Errorf("parse PDF: %w", err)
	}
	pages := reader.NumPage()
	if pages <= 0 {
		return nil, ErrNoExtractableText
	}
	if pages > opts.MaxPages {
		return nil, fmt.Errorf("%w: PDF has %d pages, limit is %d", ErrParseLimit, pages, opts.MaxPages)
	}

	remaining := opts.MaxTextBytes
	for pageNumber := 1; pageNumber <= pages; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		text, pageErr := reader.Page(pageNumber).GetPlainText(nil)
		if pageErr != nil {
			return nil, fmt.Errorf("extract PDF page %d: %w", pageNumber, pageErr)
		}
		if len(text) > remaining {
			return nil, fmt.Errorf("%w: extracted PDF text exceeds %d bytes", ErrParseLimit, opts.MaxTextBytes)
		}
		remaining -= len(text)
		for _, part := range splitFragments(text) {
			fragments = append(fragments, fragment{text: part, page: pageNumber, file: fileName})
		}
	}
	if len(fragments) == 0 {
		return nil, ErrNoExtractableText
	}
	return fragments, nil
}

func parseZIP(ctx context.Context, payload []byte, outerName string, opts Options) ([]fragment, string, error) {
	archive, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return nil, "", fmt.Errorf("open ZIP: %w", err)
	}
	if len(archive.File) > opts.MaxArchiveEntries {
		return nil, "", fmt.Errorf("%w: archive has %d entries, limit is %d", ErrParseLimit, len(archive.File), opts.MaxArchiveEntries)
	}

	budget := &parseBudget{textRemaining: opts.MaxTextBytes, xmlRemaining: int64(opts.MaxXMLBytes)}
	if findZipEntry(archive.File, "word/document.xml") != nil {
		fragments, err := parseDOCXArchive(ctx, archive, outerName, opts, budget)
		return fragments, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", err
	}

	var fragments []fragment
	totalBytes := int64(0)
	documents := 0
	for _, entry := range archive.File {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Ext(entry.Name), ".docx") {
			continue
		}
		documents++
		if documents > opts.MaxArchiveDocuments {
			return nil, "", fmt.Errorf("%w: more than %d DOCX files in archive", ErrParseLimit, opts.MaxArchiveDocuments)
		}
		if entry.UncompressedSize64 > uint64(opts.MaxDownloadBytes) {
			return nil, "", fmt.Errorf("%w: nested DOCX %q is too large", ErrParseLimit, safeDisplayName(entry.Name))
		}
		nested, readErr := readZipEntry(entry, opts.MaxDownloadBytes)
		if readErr != nil {
			return nil, "", fmt.Errorf("read nested DOCX %q: %w", safeDisplayName(entry.Name), readErr)
		}
		totalBytes += int64(len(nested))
		if totalBytes > opts.MaxDownloadBytes {
			return nil, "", fmt.Errorf("%w: expanded archive exceeds %d bytes", ErrParseLimit, opts.MaxDownloadBytes)
		}
		if !isZIP(nested) {
			return nil, "", fmt.Errorf("%w: %q is not a DOCX ZIP", ErrUnsupportedFormat, safeDisplayName(entry.Name))
		}
		docx, openErr := zip.NewReader(bytes.NewReader(nested), int64(len(nested)))
		if openErr != nil {
			return nil, "", fmt.Errorf("open nested DOCX %q: %w", safeDisplayName(entry.Name), openErr)
		}
		parts, parseErr := parseDOCXArchive(ctx, docx, safeDisplayName(entry.Name), opts, budget)
		if parseErr != nil {
			return nil, "", fmt.Errorf("parse nested DOCX %q: %w", safeDisplayName(entry.Name), parseErr)
		}
		fragments = append(fragments, parts...)
	}
	if documents == 0 {
		return nil, "", fmt.Errorf("%w: ZIP does not contain DOCX documents", ErrUnsupportedFormat)
	}
	if len(fragments) == 0 {
		return nil, "", ErrNoExtractableText
	}
	return fragments, "application/zip", nil
}

type parseBudget struct {
	textRemaining int
	xmlRemaining  int64
}

func (b *parseBudget) consumeText(size int, limit int) error {
	if size < 0 || size > b.textRemaining {
		return fmt.Errorf("%w: extracted DOCX text exceeds %d bytes across the archive", ErrParseLimit, limit)
	}
	b.textRemaining -= size
	return nil
}

func (b *parseBudget) consumeXML(size int64, limit int) error {
	if size < 0 || size > b.xmlRemaining {
		return fmt.Errorf("%w: expanded DOCX XML exceeds %d bytes across the archive", ErrParseLimit, limit)
	}
	b.xmlRemaining -= size
	return nil
}

func parseDOCXArchive(ctx context.Context, archive *zip.Reader, fileName string, opts Options, budget *parseBudget) ([]fragment, error) {
	if len(archive.File) > opts.MaxArchiveEntries {
		return nil, fmt.Errorf("%w: DOCX has %d entries, limit is %d", ErrParseLimit, len(archive.File), opts.MaxArchiveEntries)
	}
	documentXML := findZipEntry(archive.File, "word/document.xml")
	if documentXML == nil {
		return nil, fmt.Errorf("%w: DOCX has no word/document.xml", ErrUnsupportedFormat)
	}
	if documentXML.UncompressedSize64 > uint64(budget.xmlRemaining) {
		return nil, fmt.Errorf("%w: expanded DOCX XML exceeds %d bytes across the archive", ErrParseLimit, opts.MaxXMLBytes)
	}
	xmlPayload, err := readZipEntry(documentXML, budget.xmlRemaining)
	if err != nil {
		return nil, fmt.Errorf("read DOCX XML: %w", err)
	}
	if err := budget.consumeXML(int64(len(xmlPayload)), opts.MaxXMLBytes); err != nil {
		return nil, err
	}
	return parseWordXML(ctx, bytes.NewReader(xmlPayload), fileName, opts.MaxTextBytes, budget)
}

func findZipEntry(files []*zip.File, wanted string) *zip.File {
	for _, entry := range files {
		if strings.EqualFold(strings.ReplaceAll(entry.Name, "\\", "/"), wanted) {
			return entry
		}
	}
	return nil
}

func readZipEntry(entry *zip.File, maxBytes int64) ([]byte, error) {
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrParseLimit
	}
	return data, nil
}

func parseWordXML(ctx context.Context, reader io.Reader, fileName string, maxBytes int, budget *parseBudget) ([]fragment, error) {
	decoder := xml.NewDecoder(reader)
	var fragments []fragment
	var paragraph strings.Builder
	var style string
	var section string
	inParagraph := false
	inText := false

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode DOCX XML: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			switch value.Name.Local {
			case "p":
				inParagraph = true
				paragraph.Reset()
				style = ""
			case "t":
				if inParagraph {
					inText = true
				}
			case "tab", "br":
				if inParagraph {
					paragraph.WriteByte(' ')
				}
			case "pStyle":
				for _, attr := range value.Attr {
					if attr.Name.Local == "val" {
						style = attr.Value
						break
					}
				}
			}
		case xml.CharData:
			if inParagraph && inText {
				paragraph.Write([]byte(value))
			}
		case xml.EndElement:
			switch value.Name.Local {
			case "t":
				inText = false
			case "p":
				inParagraph = false
				text := collapseWhitespace(paragraph.String())
				if text == "" {
					continue
				}
				if err := budget.consumeText(len(text), maxBytes); err != nil {
					return nil, err
				}
				if looksLikeHeading(style, text) {
					section = boundedMetadata(text, 240)
				}
				for _, part := range splitFragments(text) {
					fragments = append(fragments, fragment{text: part, file: fileName, section: section})
				}
			}
		}
	}
	if len(fragments) == 0 {
		return nil, ErrNoExtractableText
	}
	return fragments, nil
}

func looksLikeHeading(style, text string) bool {
	style = strings.ToLower(style)
	lower := strings.ToLower(text)
	if strings.Contains(style, "heading") || strings.Contains(style, "title") || strings.Contains(style, "заголов") {
		return true
	}
	return strings.HasPrefix(lower, "пм.") || strings.HasPrefix(lower, "пм ") ||
		strings.HasPrefix(lower, "мдк.") || strings.HasPrefix(lower, "мдк ") ||
		strings.Contains(lower, "профессиональный модуль") || strings.Contains(lower, "учебная дисциплина")
}

func splitFragments(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	const maxRunes = 1200
	var result []string
	var current strings.Builder
	currentRunes := 0
	flush := func() {
		value := collapseWhitespace(current.String())
		if value != "" {
			result = append(result, value)
		}
		current.Reset()
		currentRunes = 0
	}
	for _, raw := range lines {
		line := collapseWhitespace(raw)
		if line == "" {
			flush()
			continue
		}
		lineRunes := []rune(line)
		for len(lineRunes) > maxRunes {
			flush()
			result = append(result, string(lineRunes[:maxRunes]))
			lineRunes = lineRunes[maxRunes:]
		}
		line = string(lineRunes)
		separator := 0
		if currentRunes > 0 {
			separator = 1
		}
		if currentRunes+len(lineRunes)+separator > maxRunes {
			flush()
		}
		if current.Len() > 0 {
			current.WriteByte(' ')
			currentRunes++
		}
		current.WriteString(line)
		currentRunes += len(lineRunes)
	}
	flush()
	return result
}

func boundedMetadata(value string, limit int) string {
	runes := []rune(collapseWhitespace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit-1]) + "…"
}

func collapseWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
