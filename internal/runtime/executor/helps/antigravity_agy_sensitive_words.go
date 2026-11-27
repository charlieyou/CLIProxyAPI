package helps

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/MoeForge/moego/ojson"
	"github.com/MoeForge/moego/regexp"
)

// AgySensitiveWordMatcher is the agy build's sensitive-word matcher for
// Antigravity system instructions. It inserts U+200B after the first rune of
// each match and writes the result without HTML escaping, matching the agy
// wire bytes. The shared SensitiveWordMatcher keeps serving other providers.
type AgySensitiveWordMatcher struct {
	regex *regexp.Regexp
}

// BuildAgySensitiveWordMatcher compiles a case-insensitive regex from the word
// list. Words are sorted by length (longest first) for proper matching.
func BuildAgySensitiveWordMatcher(words []string) *AgySensitiveWordMatcher {
	if len(words) == 0 {
		return nil
	}

	var validWords []string
	for _, w := range words {
		w = strings.TrimSpace(w)
		if utf8.RuneCountInString(w) >= 2 && !strings.Contains(w, zeroWidthSpace) {
			validWords = append(validWords, w)
		}
	}
	if len(validWords) == 0 {
		return nil
	}

	sort.Slice(validWords, func(i, j int) bool {
		return len(validWords[i]) > len(validWords[j])
	})

	escaped := make([]string, len(validWords))
	for i, w := range validWords {
		escaped[i] = regexp.QuoteMeta(w)
	}

	re, err := regexp.Compile("(?i)" + strings.Join(escaped, "|"))
	if err != nil {
		return nil
	}
	return &AgySensitiveWordMatcher{regex: re}
}

// agyObfuscateWord inserts a zero-width space after the first rune.
func agyObfuscateWord(word string) string {
	if strings.Contains(word, zeroWidthSpace) {
		return word
	}
	r, size := utf8.DecodeRuneInString(word)
	if r == utf8.RuneError || size >= len(word) {
		return word
	}
	return string(r) + zeroWidthSpace + word[size:]
}

func (m *AgySensitiveWordMatcher) obfuscateText(text string) string {
	if m == nil || m.regex == nil {
		return text
	}
	return m.regex.ReplaceAllStringFunc(text, agyObfuscateWord)
}

// AgyObfuscateSensitiveWordsInSystemInstruction obfuscates sensitive words in an
// Antigravity system instruction.
func AgyObfuscateSensitiveWordsInSystemInstruction(payload []byte, matcher *AgySensitiveWordMatcher) []byte {
	if matcher == nil || matcher.regex == nil {
		return payload
	}

	for _, path := range []string{"request.systemInstruction", "request.system_instruction"} {
		instruction := ojson.GetBytes(payload, path)
		if !instruction.Exists() {
			continue
		}
		if instruction.Type == ojson.String {
			text := instruction.String()
			if obfuscated := matcher.obfuscateText(text); obfuscated != text {
				payload = setAntigravityJSONStringNoEscape(payload, path, obfuscated)
			}
			continue
		}

		parts := instruction.Get("parts")
		if !parts.IsArray() {
			continue
		}
		parts.ForEach(func(key, part ojson.Result) bool {
			if part.Get("text").Type != ojson.String {
				return true
			}
			text := part.Get("text").String()
			if obfuscated := matcher.obfuscateText(text); obfuscated != text {
				payload = setAntigravityJSONStringNoEscape(payload, path+".parts."+key.String()+".text", obfuscated)
			}
			return true
		})
	}

	return payload
}
