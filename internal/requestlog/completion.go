package requestlog

import (
	"bytes"
	"encoding/json"
)

// Track protocol completion independently of the bounded body snapshot. A large
// text/tool event may exceed 512 KiB, but the small terminal events still decide
// whether a subsequent client disconnect interrupted the response.
type streamCompletion struct {
	protocol            string
	line                []byte
	skipping            bool
	finishReason        string
	finished            bool
	finishedWhileActive bool
	failed              bool
}

const completionLineLimit = 8 << 10

func (s *streamCompletion) write(data []byte, active bool) {
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		part := data
		if end >= 0 {
			part = data[:end]
		}
		if !s.skipping {
			if len(s.line)+len(part) > completionLineLimit {
				s.skipping = true
				s.line = s.line[:0]
			} else {
				s.line = append(s.line, part...)
			}
		}
		if end < 0 {
			return
		}
		if !s.skipping {
			s.consume(s.line, active)
		}
		s.line, s.skipping = s.line[:0], false
		data = data[end+1:]
	}
}

func (s *streamCompletion) consume(line []byte, active bool) {
	line = bytes.TrimSpace(line)
	if bytes.Equal(line, []byte("event: error")) {
		s.failed = true
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[len("data:"):])
	terminal := false
	if bytes.Equal(data, []byte("[DONE]")) {
		terminal = s.protocol == "openai_chat" && s.finishReason != ""
	} else {
		var value struct {
			Type    string          `json:"type"`
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal(data, &value) != nil {
			return
		}
		if value.Type == "error" || (len(value.Error) > 0 && string(value.Error) != "null") {
			s.failed = true
		}
		if s.protocol == "openai_chat" {
			for _, choice := range value.Choices {
				if choice.FinishReason != "" {
					s.finishReason = choice.FinishReason
				}
			}
		} else if s.protocol == "anthropic_messages" {
			if value.Type == "message_delta" && value.Delta.StopReason != "" {
				s.finishReason = value.Delta.StopReason
			}
			terminal = value.Type == "message_stop" && s.finishReason != ""
		}
	}
	if terminal && !s.finished {
		s.finished, s.finishedWhileActive = true, active
	}
}

func (s *streamCompletion) complete() bool {
	return s.finished && s.finishedWhileActive && !s.failed
}
