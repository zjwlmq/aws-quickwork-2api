package proxy

import "strings"

// Ordinary text streams immediately. Buffer from the first native link until
// the generated files have been downloaded and their public links are ready.
type mediaStream struct {
	pending string
	emit    func(string)
}

func (m *mediaStream) push(text string, final bool, rewrite func(string) string) {
	m.pending += text
	const marker = "qw-file://"
	for m.pending != "" {
		index := strings.Index(m.pending, marker)
		if index < 0 {
			keep := 0
			if !final {
				for n := 1; n < len(marker) && n <= len(m.pending); n++ {
					if strings.HasSuffix(m.pending, marker[:n]) {
						keep = n
					}
				}
			}
			if out := m.pending[:len(m.pending)-keep]; out != "" {
				m.emit(out)
			}
			m.pending = m.pending[len(m.pending)-keep:]
			return
		}
		if index > 0 {
			m.emit(m.pending[:index])
			m.pending = m.pending[index:]
		}
		if !final {
			return
		}
		end := strings.IndexAny(m.pending[len(marker):], " \t\n\r)\"'<>`]")
		if end < 0 {
			end = len(m.pending) - len(marker)
		}
		end += len(marker)
		m.emit(rewrite(m.pending[:end]))
		m.pending = m.pending[end:]
	}
}
