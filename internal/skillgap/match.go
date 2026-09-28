package skillgap

import (
	"math"
	"strings"
	"unicode"
)

const (
	foundMessage    = "Навык найден в анализируемом федеральном документе."
	partialMessage  = "В документе найдены связанные формулировки; требуется экспертная проверка."
	notFoundMessage = "Не найдено в анализируемом документе. Это не означает, что конкретная образовательная организация не обучает этому навыку."
)

type candidate struct {
	status     MatchStatus
	matchedBy  string
	confidence float64
	fragment   fragment
}

type indexedFragment struct {
	source     fragment
	normalized string
	tokens     map[string]struct{}
}

func matchSkills(skills []MarketSkill, fragments []fragment, evidenceLimit int) []SkillMatch {
	indexed := make([]indexedFragment, 0, len(fragments))
	for _, source := range fragments {
		normalized := normalizeText(source.text)
		if normalized == "" {
			continue
		}
		tokens := make(map[string]struct{})
		for _, token := range strings.Fields(normalized) {
			if isMeaningfulToken(token) {
				tokens[token] = struct{}{}
			}
		}
		indexed = append(indexed, indexedFragment{source: source, normalized: normalized, tokens: tokens})
	}
	result := make([]SkillMatch, 0, len(skills))
	for _, skill := range skills {
		match := SkillMatch{Skill: strings.TrimSpace(skill.Name), Status: StatusNotFound, Message: notFoundMessage}
		candidates := findCandidates(skill, indexed)
		if len(candidates) == 0 {
			result = append(result, match)
			continue
		}

		best := candidates[0]
		for _, item := range candidates[1:] {
			if statusRank(item.status) > statusRank(best.status) ||
				(item.status == best.status && item.confidence > best.confidence) {
				best = item
			}
		}
		match.Status = best.status
		match.MatchedBy = best.matchedBy
		match.Confidence = best.confidence
		if best.status == StatusFound {
			match.Message = foundMessage
		} else {
			match.Message = partialMessage
		}

		seen := make(map[string]struct{})
		for _, item := range candidates {
			if item.status != best.status || len(match.Evidence) >= evidenceLimit {
				continue
			}
			evidence := evidenceFrom(item.fragment, item.matchedBy)
			key := evidence.File + "|" + evidence.Section + "|" + evidence.Excerpt
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			match.Evidence = append(match.Evidence, evidence)
		}
		result = append(result, match)
	}
	return result
}

func findCandidates(skill MarketSkill, fragments []indexedFragment) []candidate {
	fullTerms := uniqueNormalized(append([]string{skill.Name}, skill.Aliases...))
	partialTerms := uniqueNormalized(skill.PartialTerms)
	var candidates []candidate
	for _, item := range fragments {
		normalizedFragment := item.normalized
		matched := false
		for _, term := range fullTerms {
			if phrasePresent(normalizedFragment, term) {
				candidates = append(candidates, candidate{status: StatusFound, matchedBy: term, confidence: 1, fragment: item.source})
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		for _, term := range partialTerms {
			if phrasePresent(normalizedFragment, term) {
				candidates = append(candidates, candidate{status: StatusPartial, matchedBy: term, confidence: 0.65, fragment: item.source})
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		if related, confidence := tokenOverlap(fullTerms, item.tokens); related {
			candidates = append(candidates, candidate{status: StatusPartial, matchedBy: normalizeText(skill.Name), confidence: confidence, fragment: item.source})
		}
	}
	return candidates
}

func tokenOverlap(terms []string, fragmentTokens map[string]struct{}) (bool, float64) {
	best := 0.0
	for _, term := range terms {
		tokens := meaningfulTokens(term)
		if len(tokens) < 2 {
			continue
		}
		hits := 0
		for _, token := range tokens {
			if _, ok := fragmentTokens[token]; ok {
				hits++
			}
		}
		ratio := float64(hits) / float64(len(tokens))
		minimumHits := 2
		if len(tokens) >= 5 {
			minimumHits = 3
		}
		if hits >= minimumHits && ratio >= 0.6 {
			best = math.Max(best, math.Min(0.85, ratio))
		}
	}
	return best > 0, best
}

func normalizeText(value string) string {
	value = strings.ToLower(strings.ReplaceAll(value, "ё", "е"))
	var b strings.Builder
	space := true
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '+' || r == '#' {
			b.WriteRune(r)
			space = false
			continue
		}
		if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

func phrasePresent(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	return strings.Contains(" "+haystack+" ", " "+needle+" ")
}

func uniqueNormalized(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		normalized := normalizeText(value)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	return result
}

func meaningfulTokens(value string) []string {
	var result []string
	for _, token := range strings.Fields(value) {
		if isMeaningfulToken(token) {
			result = append(result, token)
		}
	}
	return result
}

func isMeaningfulToken(token string) bool {
	if len([]rune(token)) < 3 {
		return false
	}
	switch token {
	case "для", "при", "как", "или", "над", "под", "the", "and", "with":
		return false
	default:
		return true
	}
}

func evidenceFrom(source fragment, matchedBy string) Evidence {
	return Evidence{
		Excerpt: excerptAround(source.text, matchedBy, 360),
		Page:    source.page,
		File:    source.file,
		Section: source.section,
	}
}

func excerptAround(text, normalizedNeedle string, maxRunes int) string {
	text = collapseWhitespace(text)
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	normalized := normalizeText(text)
	index := strings.Index(normalized, normalizedNeedle)
	if index < 0 {
		return string(runes[:maxRunes-1]) + "…"
	}
	// The normalized byte offset is only an approximation for the original
	// Unicode string, so centre the excerpt proportionally and keep it bounded.
	ratio := float64(index) / float64(max(1, len(normalized)))
	centre := int(ratio * float64(len(runes)))
	start := centre - maxRunes/2
	if start < 0 {
		start = 0
	}
	end := start + maxRunes
	if end > len(runes) {
		end = len(runes)
		start = max(0, end-maxRunes)
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(runes) {
		suffix = "…"
	}
	return prefix + string(runes[start:end]) + suffix
}

func statusRank(status MatchStatus) int {
	switch status {
	case StatusFound:
		return 2
	case StatusPartial:
		return 1
	default:
		return 0
	}
}
