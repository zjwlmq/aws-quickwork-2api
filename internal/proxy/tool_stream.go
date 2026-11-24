package proxy

import (
	"errors"
	"strings"
	"unicode"
)

// clientToolStream separates prose from the client tool transport. Parsed
// calls stay in order and can be sent after the native turn succeeds.
type clientToolStream struct {
	allowed  map[string]bool
	emitText func(string)
	calls    []ToolCallReply

	checkBare bool
	leading   strings.Builder
	bare      *bareToolJSON

	openTail string
	inCall   bool
	payload  strings.Builder
	closeAt  int
}

func (s *clientToolStream) push(delta string) {
	if s.checkBare {
		s.leading.WriteString(delta)
		text := s.leading.String()
		if s.bare == nil {
			start, fenced, waiting := bareToolStart(text)
			if waiting {
				return
			}
			if start >= 0 {
				s.bare = &bareToolJSON{start: start, scanned: start, fenced: fenced}
			}
		}
		if s.bare != nil && s.bare.couldBeCall(text, s.allowed) {
			return
		}
		delta = text
		s.leading.Reset()
		s.checkBare = false
		s.bare = nil
	}
	s.pushTagged(delta)
}

func (s *clientToolStream) pushTagged(delta string) {
	delta = s.openTail + delta
	s.openTail = ""
	for delta != "" {
		if !s.inCall {
			start := strings.Index(delta, clientToolCallOpen)
			if start < 0 {
				// An opening tag can be split anywhere across native deltas.
				keep := min(len(delta), len(clientToolCallOpen)-1)
				for keep > 0 && !strings.HasSuffix(delta, clientToolCallOpen[:keep]) {
					keep--
				}
				end := len(delta) - keep
				s.openTail = strings.Clone(delta[end:])
				if end > 0 {
					s.emitText(delta[:end])
				}
				return
			}
			if start > 0 {
				s.emitText(delta[:start])
			}
			s.inCall = true
			delta = delta[start+len(clientToolCallOpen):]
			continue
		}

		s.payload.WriteString(delta)
		text := s.payload.String()
		for {
			closeOffset := strings.Index(text[s.closeAt:], clientToolCallClose)
			if closeOffset < 0 {
				s.closeAt = max(s.closeAt, len(text)-len(clientToolCallClose)+1)
				return
			}
			end := s.closeAt + closeOffset
			s.closeAt = end + len(clientToolCallClose)
			envelopes := parseToolEnvelopes(text[:end])
			if envelopes == nil {
				// A literal closing tag can be part of a string argument.
				continue
			}
			s.calls = append(s.calls, repliesFromEnvelopes(envelopes, s.allowed)...)
			delta = text[s.closeAt:]
			s.payload.Reset()
			s.inCall, s.closeAt = false, 0
			break
		}
	}
}

func (s *clientToolStream) finish() ([]ToolCallReply, error) {
	if s.checkBare {
		text := s.leading.String()
		s.leading.Reset()
		_, calls := extractBareToolCall(text, s.allowed)
		if len(calls) > 0 {
			return calls, nil
		}
		s.pushTagged(text)
	}
	if s.inCall {
		return nil, errors.New("upstream returned an incomplete or invalid client tool call")
	}
	if s.openTail != "" {
		s.emitText(s.openTail)
	}
	return s.calls, nil
}

// The legacy fallback also accepts a bare JSON call, optionally fenced in
// Markdown. Only defer leading text while it could still be that envelope.
func bareToolStart(text string) (offset int, fenced, waiting bool) {
	const fence = "```"
	rest := strings.TrimLeftFunc(text, unicode.IsSpace)
	if len(rest) < len(fence) && strings.HasPrefix(fence, rest) {
		return -1, false, true
	}
	if strings.HasPrefix(rest, fence) {
		lineEnd := strings.IndexByte(rest, '\n')
		if lineEnd < 0 {
			return -1, false, true
		}
		rest = strings.TrimLeftFunc(rest[lineEnd+1:], unicode.IsSpace)
		fenced = true
	}
	if rest == "" {
		return -1, fenced, true
	}
	if rest[0] != '{' && rest[0] != '[' {
		return -1, fenced, false
	}
	next := strings.TrimLeftFunc(rest[1:], unicode.IsSpace)
	if next == "" {
		return -1, fenced, true
	}
	if (rest[0] == '{' && next[0] != '"') || (rest[0] == '[' && next[0] != '{') {
		return -1, fenced, false
	}
	return len(text) - len(rest), fenced, false
}

type bareToolJSON struct {
	start, scanned, end, depth int
	fenced, quoted, escaped    bool
}

func (b *bareToolJSON) couldBeCall(text string, allowed map[string]bool) bool {
	for b.end == 0 && b.scanned < len(text) {
		ch := text[b.scanned]
		b.scanned++
		if b.quoted {
			if b.escaped {
				b.escaped = false
			} else if ch == '\\' {
				b.escaped = true
			} else if ch == '"' {
				b.quoted = false
			}
			continue
		}
		switch ch {
		case '"':
			b.quoted = true
		case '{', '[':
			b.depth++
		case '}', ']':
			b.depth--
			if b.depth == 0 {
				b.end = b.scanned
				matched := false
				for _, envelope := range parseToolEnvelopes(text[b.start:b.end]) {
					matched = matched || allowed[strings.TrimSpace(envelope.Name)]
				}
				if !matched {
					return false
				}
			}
		}
	}
	if b.end == 0 {
		return true
	}
	tail := strings.TrimSpace(text[b.end:])
	return tail == "" || (b.fenced && strings.HasPrefix("```", tail))
}
