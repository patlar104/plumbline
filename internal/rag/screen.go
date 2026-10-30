package rag

import (
	"regexp"
	"strings"
)

// injectionPatterns match sentences that try to instruct the assistant rather
// than inform the reader. Pattern screening is a first line of defence only;
// fencing, the output leak check and the evaluation set back it up.
var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override)\b[^.!?\n]{0,40}\b(previous|prior|above|earlier|all|any|your|the)\b[^.!?\n]{0,30}\b(instructions?|prompts?|rules|directions|guidelines)\b`),
	regexp.MustCompile(`(?i)\byou are now\b`),
	regexp.MustCompile(`(?i)^\W*(system|assistant|developer)\s*:`),
	regexp.MustCompile(`(?i)<\s*/?\s*(script|system|instructions?)\b`),
	regexp.MustCompile(`(?i)\b(reveal|print|show|repeat|output|leak)\b[^.!?\n]{0,30}\b(system prompt|hidden instructions|your instructions|your prompt)\b`),
	regexp.MustCompile(`(?i)\bnew instructions?\s*:`),
	regexp.MustCompile(`(?i)\bdo not (tell|inform|alert) the user\b`),
}

var (
	zeroWidth   = strings.NewReplacer("\u200b", "", "\u200c", "", "\u200d", "", "\u2060", "", "\ufeff", "", "\u00ad", "")
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlTag     = regexp.MustCompile(`</?[A-Za-z][A-Za-z0-9-]*(\s[^<>]{0,200})?/?>`)
	spaces      = regexp.MustCompile(`[ \t]+`)
)

// IsInjection reports whether a single sentence looks like an instruction
// aimed at the model.
func IsInjection(sentence string) bool {
	s := zeroWidth.Replace(sentence)
	for _, re := range injectionPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// Screen removes sentences that look like injected instructions, hidden HTML
// comments and markup. It returns the cleaned text and what it removed.
// Line structure is preserved; removed sentences are dropped in place.
func Screen(text string) (clean string, removed []string) {
	text = zeroWidth.Replace(text)
	text = htmlComment.ReplaceAllStringFunc(text, func(c string) string {
		inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(c, "<!--"), "-->"))
		if inner != "" {
			removed = append(removed, inner)
		}
		return " "
	})
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		var kept []string
		for _, s := range Sentences(line) {
			// Check the raw sentence (catches <script>) and the stripped one,
			// which is what the model would actually read: "<b>SYSTEM: ..."
			// only looks like a fake turn after its tag is gone.
			stripped := strings.TrimSpace(spaces.ReplaceAllString(htmlTag.ReplaceAllString(s, " "), " "))
			if IsInjection(s) || IsInjection(stripped) {
				removed = append(removed, s)
				continue
			}
			if stripped != "" {
				kept = append(kept, stripped)
			}
		}
		if len(kept) > 0 {
			lines = append(lines, strings.Join(kept, " "))
		}
	}
	return strings.Join(lines, "\n"), removed
}
