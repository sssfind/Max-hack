package skillgap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	defaultTimeout             = 15 * time.Second
	defaultPDFParseTimeout     = 15 * time.Second
	defaultPDFMaxMemoryBytes   = 512 << 20
	defaultCacheTTL            = 30 * time.Minute
	defaultMaxDownloadBytes    = 20 << 20
	defaultMaxTextBytes        = 5 << 20
	defaultMaxXMLBytes         = 32 << 20
	defaultMaxPages            = 300
	defaultMaxArchiveEntries   = 1000
	defaultMaxArchiveDocuments = 32
	defaultMaxCacheEntries     = 64
	defaultMaxCacheBytes       = 32 << 20
	defaultMaxEvidence         = 3
	defaultMaxSkills           = 100
	defaultMaxTermsPerSkill    = 32
	defaultMaxTermRunes        = 256
)

type fragment struct {
	text    string
	page    int
	file    string
	section string
}

type parsedDocument struct {
	fragments []fragment
	mediaType string
}

type cacheEntry struct {
	hash      string
	document  parsedDocument
	fetchedAt time.Time
	mediaType string
	fileName  string
	lastUsed  time.Time
	sizeBytes int64
}

// Analyzer is safe for concurrent use.
type Analyzer struct {
	opts      Options
	client    *http.Client
	mu        sync.Mutex
	urlCache  map[string]cacheEntry
	hashCache map[string]cacheEntry
}

// New constructs a bounded document analyzer.
func New(opts Options) *Analyzer {
	applyDefaults(&opts)
	baseClient := opts.HTTPClient
	if baseClient == nil {
		baseClient = &http.Client{}
	}
	clientCopy := *baseClient
	clientCopy.Timeout = opts.Timeout
	clientCopy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		_, err := validateSourceURL(req.URL.String())
		return err
	}

	return &Analyzer{
		opts:      opts,
		client:    &clientCopy,
		urlCache:  make(map[string]cacheEntry),
		hashCache: make(map[string]cacheEntry),
	}
}

func applyDefaults(opts *Options) {
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.PDFParseTimeout <= 0 {
		opts.PDFParseTimeout = defaultPDFParseTimeout
	}
	if opts.PDFMaxMemoryBytes <= 0 {
		opts.PDFMaxMemoryBytes = defaultPDFMaxMemoryBytes
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = defaultCacheTTL
	}
	if opts.MaxDownloadBytes <= 0 {
		opts.MaxDownloadBytes = defaultMaxDownloadBytes
	}
	if opts.MaxTextBytes <= 0 {
		opts.MaxTextBytes = defaultMaxTextBytes
	}
	if opts.MaxXMLBytes <= 0 {
		opts.MaxXMLBytes = defaultMaxXMLBytes
	}
	if opts.MaxPages <= 0 {
		opts.MaxPages = defaultMaxPages
	}
	if opts.MaxArchiveEntries <= 0 {
		opts.MaxArchiveEntries = defaultMaxArchiveEntries
	}
	if opts.MaxArchiveDocuments <= 0 {
		opts.MaxArchiveDocuments = defaultMaxArchiveDocuments
	}
	if opts.MaxCacheEntries <= 0 {
		opts.MaxCacheEntries = defaultMaxCacheEntries
	}
	if opts.MaxCacheBytes <= 0 {
		opts.MaxCacheBytes = defaultMaxCacheBytes
	}
	if opts.MaxEvidencePerSkill <= 0 {
		opts.MaxEvidencePerSkill = defaultMaxEvidence
	}
	if opts.MaxSkills <= 0 {
		opts.MaxSkills = defaultMaxSkills
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
}

// Analyze downloads, parses and compares a federal document with market
// skills. If Primary cannot be used, Fallback is attempted once.
func (a *Analyzer) Analyze(ctx context.Context, req Request) (Result, error) {
	if err := validateRequest(req, a.opts.MaxSkills); err != nil {
		return Result{}, err
	}

	doc, source, err := a.load(ctx, req.Primary)
	if err == nil {
		return Result{Source: source, Matches: matchSkills(req.Skills, doc.fragments, a.opts.MaxEvidencePerSkill)}, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Result{}, err
	}
	if errors.Is(err, ErrUnsafeSource) {
		return Result{}, err
	}

	attempt := SourceAttempt{Ref: req.Primary, Status: retrievalStatusForError(err), Error: err.Error()}
	if req.Fallback == nil {
		return Result{Attempts: []SourceAttempt{attempt}}, fmt.Errorf("load primary document: %w", err)
	}

	fallbackDoc, fallbackSource, fallbackErr := a.load(ctx, *req.Fallback)
	if fallbackErr != nil {
		attempts := []SourceAttempt{
			attempt,
			{Ref: *req.Fallback, Status: retrievalStatusForError(fallbackErr), Error: fallbackErr.Error()},
		}
		return Result{Attempts: attempts}, errors.Join(
			fmt.Errorf("load primary document: %w", err),
			fmt.Errorf("load fallback document: %w", fallbackErr),
		)
	}

	return Result{
		Source:       fallbackSource,
		UsedFallback: true,
		Attempts:     []SourceAttempt{attempt},
		Matches:      matchSkills(req.Skills, fallbackDoc.fragments, a.opts.MaxEvidencePerSkill),
	}, nil
}

func validateRequest(req Request, maxSkills int) error {
	if strings.TrimSpace(req.Primary.URL) == "" {
		return fmt.Errorf("%w: primary URL is required", ErrInvalidRequest)
	}
	if !validDocumentType(req.Primary.Type) {
		return fmt.Errorf("%w: unsupported primary document type %q", ErrInvalidRequest, req.Primary.Type)
	}
	if req.Fallback != nil {
		if strings.TrimSpace(req.Fallback.URL) == "" {
			return fmt.Errorf("%w: fallback URL is required", ErrInvalidRequest)
		}
		if !validDocumentType(req.Fallback.Type) {
			return fmt.Errorf("%w: unsupported fallback document type %q", ErrInvalidRequest, req.Fallback.Type)
		}
	}
	if len(req.Skills) == 0 || len(req.Skills) > maxSkills {
		return fmt.Errorf("%w: skills count must be between 1 and %d", ErrInvalidRequest, maxSkills)
	}
	seen := make(map[string]struct{}, len(req.Skills))
	for _, skill := range req.Skills {
		if utf8.RuneCountInString(skill.Name) > defaultMaxTermRunes {
			return fmt.Errorf("%w: skill name exceeds %d characters", ErrInvalidRequest, defaultMaxTermRunes)
		}
		if len(skill.Aliases)+len(skill.PartialTerms) > defaultMaxTermsPerSkill {
			return fmt.Errorf("%w: skill %q has more than %d alternate terms", ErrInvalidRequest, skill.Name, defaultMaxTermsPerSkill)
		}
		for _, term := range append(append([]string(nil), skill.Aliases...), skill.PartialTerms...) {
			if utf8.RuneCountInString(term) > defaultMaxTermRunes {
				return fmt.Errorf("%w: alternate term for %q exceeds %d characters", ErrInvalidRequest, skill.Name, defaultMaxTermRunes)
			}
		}
		name := normalizeText(skill.Name)
		if name == "" {
			return fmt.Errorf("%w: skill name is required", ErrInvalidRequest)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("%w: duplicate skill %q", ErrInvalidRequest, skill.Name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func validDocumentType(value DocumentType) bool {
	switch value {
	case DocumentPOP, DocumentFGOS, DocumentOther:
		return true
	default:
		return false
	}
}

func retrievalStatusForError(err error) RetrievalStatus {
	if errors.Is(err, ErrUnsupportedFormat) {
		return RetrievalUnsupported
	}
	return RetrievalFailed
}

func (a *Analyzer) load(ctx context.Context, ref DocumentRef) (parsedDocument, SourceMetadata, error) {
	normalizedURL, err := validateSourceURL(ref.URL)
	if err != nil {
		return parsedDocument{}, SourceMetadata{}, err
	}
	now := a.opts.Now().UTC()
	if entry, ok := a.cachedURL(normalizedURL, now); ok {
		return entry.document, metadataFor(ref, normalizedURL, entry, RetrievalCache, true), nil
	}

	payload, mediaType, fileName, err := a.fetch(ctx, normalizedURL)
	if err != nil {
		return parsedDocument{}, SourceMetadata{}, err
	}
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])

	if entry, ok := a.cachedHash(hash, now); ok {
		entry.document = rebindOuterFileName(entry.document, entry.fileName, fileName)
		entry.fetchedAt = now
		entry.mediaType = entry.document.mediaType
		entry.fileName = fileName
		entry.sizeBytes = parsedDocumentSize(entry.document)
		a.store(normalizedURL, hash, entry)
		return entry.document, metadataFor(ref, normalizedURL, entry, RetrievalFetched, false), nil
	}

	document, err := parseDocument(ctx, payload, mediaType, fileName, a.opts)
	if err != nil {
		return parsedDocument{}, SourceMetadata{}, err
	}
	entry := cacheEntry{
		hash:      hash,
		document:  document,
		fetchedAt: now,
		mediaType: document.mediaType,
		fileName:  fileName,
		lastUsed:  now,
	}
	entry.sizeBytes = parsedDocumentSize(entry.document)
	a.store(normalizedURL, hash, entry)
	return document, metadataFor(ref, normalizedURL, entry, RetrievalFetched, false), nil
}

func rebindOuterFileName(document parsedDocument, previous, current string) parsedDocument {
	if previous == current {
		return document
	}
	fragments := append([]fragment(nil), document.fragments...)
	for index := range fragments {
		if fragments[index].file == previous {
			fragments[index].file = current
		}
	}
	document.fragments = fragments
	return document
}

func metadataFor(ref DocumentRef, normalizedURL string, entry cacheEntry, status RetrievalStatus, fromCache bool) SourceMetadata {
	return SourceMetadata{
		URL:       normalizedURL,
		Type:      ref.Type,
		Status:    status,
		FetchedAt: entry.fetchedAt,
		SHA256:    entry.hash,
		MediaType: entry.mediaType,
		FileName:  entry.fileName,
		FromCache: fromCache,
	}
}

func (a *Analyzer) cachedURL(url string, now time.Time) (cacheEntry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.urlCache[url]
	if !ok || now.Sub(entry.fetchedAt) > a.opts.CacheTTL {
		return cacheEntry{}, false
	}
	entry.lastUsed = now
	a.urlCache[url] = entry
	if hashEntry, exists := a.hashCache[entry.hash]; exists {
		hashEntry.lastUsed = now
		a.hashCache[entry.hash] = hashEntry
	}
	return entry, true
}

func (a *Analyzer) cachedHash(hash string, now time.Time) (cacheEntry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.hashCache[hash]
	if !ok {
		return cacheEntry{}, false
	}
	entry.lastUsed = now
	a.hashCache[hash] = entry
	return entry, true
}

func (a *Analyzer) store(url, hash string, entry cacheEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if entry.sizeBytes <= 0 {
		entry.sizeBytes = parsedDocumentSize(entry.document)
	}
	a.urlCache[url] = entry
	a.hashCache[hash] = entry
	for len(a.urlCache) > a.opts.MaxCacheEntries {
		deleteOldestURL(a.urlCache)
	}
	for len(a.hashCache) > a.opts.MaxCacheEntries {
		deleteOldestHash(a.hashCache)
	}
	for cacheSize(a.urlCache)+cacheSize(a.hashCache) > a.opts.MaxCacheBytes && (len(a.urlCache) > 0 || len(a.hashCache) > 0) {
		switch {
		case len(a.urlCache) == 0:
			deleteOldestHash(a.hashCache)
		case len(a.hashCache) == 0:
			deleteOldestURL(a.urlCache)
		case oldestURLTime(a.urlCache).Before(oldestURLTime(a.hashCache)):
			deleteOldestURL(a.urlCache)
		default:
			deleteOldestHash(a.hashCache)
		}
	}
}

func parsedDocumentSize(document parsedDocument) int64 {
	var size int64
	for _, item := range document.fragments {
		size += int64(len(item.text) + len(item.file) + len(item.section) + 32)
	}
	return size + int64(len(document.mediaType))
}

func cacheSize(cache map[string]cacheEntry) int64 {
	var total int64
	for _, entry := range cache {
		total += entry.sizeBytes
	}
	return total
}

func oldestURLTime(cache map[string]cacheEntry) time.Time {
	var oldest time.Time
	for _, entry := range cache {
		if oldest.IsZero() || entry.lastUsed.Before(oldest) {
			oldest = entry.lastUsed
		}
	}
	return oldest
}

func deleteOldestURL(cache map[string]cacheEntry) {
	keys := make([]string, 0, len(cache))
	for key := range cache {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return cache[keys[i]].lastUsed.Before(cache[keys[j]].lastUsed) })
	if len(keys) > 0 {
		delete(cache, keys[0])
	}
}

func deleteOldestHash(cache map[string]cacheEntry) {
	deleteOldestURL(cache)
}

// PurgeCache removes all downloaded and parsed documents.
func (a *Analyzer) PurgeCache() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.urlCache = make(map[string]cacheEntry)
	a.hashCache = make(map[string]cacheEntry)
}
