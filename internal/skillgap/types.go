// Package skillgap downloads federal education documents and compares their
// text with skills observed in the labour market.
//
// The package deliberately reports only what was found in the analysed
// federal document. It must not be used to claim that a particular college
// does or does not teach a skill.
package skillgap

import (
	"errors"
	"net/http"
	"time"
)

const AllowedHost = "spolab.firpo.ru"

// DocumentType identifies the kind of federal education document.
type DocumentType string

const (
	DocumentPOP   DocumentType = "pop"
	DocumentFGOS  DocumentType = "fgos"
	DocumentOther DocumentType = "other"
)

// MatchStatus is a cautious assessment of a market skill against the source.
type MatchStatus string

const (
	StatusFound    MatchStatus = "found"
	StatusPartial  MatchStatus = "partial"
	StatusNotFound MatchStatus = "not_found"
)

// RetrievalStatus explains how a source was obtained or why it was skipped.
type RetrievalStatus string

const (
	RetrievalFetched     RetrievalStatus = "fetched"
	RetrievalCache       RetrievalStatus = "cache"
	RetrievalUnsupported RetrievalStatus = "unsupported"
	RetrievalFailed      RetrievalStatus = "failed"
)

// DocumentRef points to an official federal document.
type DocumentRef struct {
	URL  string       `json:"url"`
	Type DocumentType `json:"type"`
}

// MarketSkill describes one skill and optional equivalent names.
// PartialTerms should contain only formulations that are related but are not
// sufficient to claim a full match on their own.
type MarketSkill struct {
	Name         string   `json:"name"`
	Aliases      []string `json:"aliases,omitempty"`
	PartialTerms []string `json:"partial_terms,omitempty"`
}

// Request asks the analyzer to inspect Primary. Fallback is normally the
// applicable FGOS and is used if a POP cannot be downloaded or parsed.
type Request struct {
	Primary  DocumentRef   `json:"primary"`
	Fallback *DocumentRef  `json:"fallback,omitempty"`
	Skills   []MarketSkill `json:"skills"`
}

// SourceMetadata makes every result reproducible and auditable.
type SourceMetadata struct {
	URL       string          `json:"url"`
	Type      DocumentType    `json:"type"`
	Status    RetrievalStatus `json:"status"`
	FetchedAt time.Time       `json:"fetched_at"`
	SHA256    string          `json:"sha256"`
	MediaType string          `json:"media_type,omitempty"`
	FileName  string          `json:"file_name,omitempty"`
	FromCache bool            `json:"from_cache"`
}

// SourceAttempt records a failed or unsupported source without pretending
// that it contributed evidence to the result.
type SourceAttempt struct {
	Ref    DocumentRef     `json:"ref"`
	Status RetrievalStatus `json:"status"`
	Error  string          `json:"error,omitempty"`
}

// Evidence points to a bounded excerpt in a PDF page or a DOCX file/section.
type Evidence struct {
	Excerpt string `json:"excerpt"`
	Page    int    `json:"page,omitempty"`
	File    string `json:"file,omitempty"`
	Section string `json:"section,omitempty"`
}

// SkillMatch is the result for one requested market skill.
type SkillMatch struct {
	Skill      string      `json:"skill"`
	Status     MatchStatus `json:"status"`
	Message    string      `json:"message"`
	MatchedBy  string      `json:"matched_by,omitempty"`
	Confidence float64     `json:"confidence"`
	Evidence   []Evidence  `json:"evidence,omitempty"`
}

// Result contains only matches from Source. Attempts describe discarded
// primary sources when a fallback was necessary.
type Result struct {
	Source       SourceMetadata  `json:"source"`
	UsedFallback bool            `json:"used_fallback"`
	Attempts     []SourceAttempt `json:"attempts,omitempty"`
	Matches      []SkillMatch    `json:"matches"`
}

// Options bounds network, archive and parser work. Zero values use safe
// defaults. HTTPClient is primarily useful for controlled tests; redirects
// are always revalidated by the package.
type Options struct {
	HTTPClient          *http.Client
	Timeout             time.Duration
	PDFParseTimeout     time.Duration
	PDFMaxMemoryBytes   int64
	CacheTTL            time.Duration
	MaxDownloadBytes    int64
	MaxTextBytes        int
	MaxXMLBytes         int
	MaxPages            int
	MaxArchiveEntries   int
	MaxArchiveDocuments int
	MaxCacheEntries     int
	MaxCacheBytes       int64
	MaxEvidencePerSkill int
	MaxSkills           int
	Now                 func() time.Time
}

var (
	ErrInvalidRequest    = errors.New("invalid skill gap request")
	ErrUnsafeSource      = errors.New("unsafe document source")
	ErrDownloadTooLarge  = errors.New("document download exceeds limit")
	ErrUnsupportedFormat = errors.New("unsupported document format")
	ErrParseLimit        = errors.New("document parsing limit exceeded")
	ErrNoExtractableText = errors.New("document contains no extractable text")
)
