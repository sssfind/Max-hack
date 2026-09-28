package bot

import "unicode/utf8"

// withGuaranteedFooter keeps provenance and user-facing limitations intact
// when a detailed result has to fit into MAX's message limit. Only the
// variable-length body is shortened.
func withGuaranteedFooter(body, footer string, limit int) string {
	if limit <= 0 {
		return ""
	}
	footerRunes := []rune(footer)
	if len(footerRunes) >= limit {
		return string(footerRunes[:limit])
	}

	available := limit - len(footerRunes)
	if utf8.RuneCountInString(body) > available {
		bodyRunes := []rune(body)
		switch available {
		case 0:
			body = ""
		case 1:
			body = "…"
		default:
			body = string(bodyRunes[:available-1]) + "…"
		}
	}
	return body + footer
}
